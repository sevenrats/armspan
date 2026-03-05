package gormspan

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errIntercepted is a sentinel error injected into the GORM callback
// chain to prevent the default gorm:create / gorm:update / gorm:delete
// processor from executing. It is cleared immediately after in the
// corresponding "clear" callback so the caller never sees it.
var errIntercepted = errors.New("gormspan: write intercepted")

// httpClient is a shared HTTP client with sensible timeouts.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

// SetHTTPClient allows replacing the default HTTP client, e.g. for
// testing or to inject custom transports / TLS configuration.
func SetHTTPClient(c *http.Client) {
	if c == nil {
		panic("gormspan: SetHTTPClient called with nil")
	}
	httpClient = c
}

// ---------------------------------------------------------------------------
// Context-based bypass
// ---------------------------------------------------------------------------

type ctxKey string

const directWriteKey ctxKey = "gormspan_direct_write"

// WithDirectWrite returns a context that tells gormspan to bypass write
// interception and let the operation hit the database directly.
//
// Use this in the business-logic service when manifesting processed
// results back into the database:
//
//	ctx := gormspan.WithDirectWrite(context.Background())
//	db.WithContext(ctx).Create(&record)  // hits DB directly
func WithDirectWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, directWriteKey, true)
}

func isDirectWrite(ctx context.Context) bool {
	v, _ := ctx.Value(directWriteKey).(bool)
	return v
}

// ---------------------------------------------------------------------------
// ManifestChange — the JSON payload POSTed to the endpoint
// ---------------------------------------------------------------------------

// ManifestChange is the JSON payload POSTed to the configured endpoint
// for every intercepted write. It matches the Python DTO:
//
//	@dataclass
//	class ManifestChange(DataClass):
//	    db: str
//	    table: str
//	    data: dict
type ManifestChange struct {
	// DB is the logical database name (from GORMSPAN_DB).
	DB string `json:"db"`

	// Table is the database table name. For delete operations this is
	// "{original_table}_tombstone".
	Table string `json:"table"`

	// Data is the JSON-serialized row. For tombstones this contains
	// {id, row, _created}.
	Data map[string]any `json:"data"`
}

// ---------------------------------------------------------------------------
// Callback registration
// ---------------------------------------------------------------------------

// registerInterceptCallbacks sets up Before/After callbacks for
// gorm:create, gorm:update, and gorm:delete. The "before" callback
// serializes the write as a ManifestChange, POSTs it to the endpoint,
// serializes the write as a ManifestChange, POSTs it to the endpoint,
// and injects the sentinel error. The "after" callback clears the
// sentinel so the caller sees a clean success.
//
// IMPORTANT: The intercept callbacks are registered BEFORE
// gorm:begin_transaction (not before gorm:create). This ensures
// GORM never acquires a SQLite write lock for intercepted operations.
// Otherwise the synchronous POST to the Python endpoint would deadlock:
// Go holds the SQLite lock waiting for the HTTP response, while Python
// tries to INSERT into the same database.
func registerInterceptCallbacks(db *gorm.DB, cfg Config) {
	if !cfg.InterceptEnabled || len(cfg.InterceptTables) == 0 {
		return
	}

	// ── Create ──
	// Intercept before the transaction starts so no lock is acquired.
	db.Callback().Create().Before("gorm:begin_transaction").
		Register("gormspan:intercept_create", makeInterceptFn("create", cfg, db))
	db.Callback().Create().After("gorm:commit_or_rollback_transaction").
		Register("gormspan:clear_intercept_create", clearInterceptFn)

	// ── Update ──
	db.Callback().Update().Before("gorm:begin_transaction").
		Register("gormspan:intercept_update", makeInterceptFn("update", cfg, db))
	db.Callback().Update().After("gorm:commit_or_rollback_transaction").
		Register("gormspan:clear_intercept_update", clearInterceptFn)

	// ── Delete ──
	db.Callback().Delete().Before("gorm:begin_transaction").
		Register("gormspan:intercept_delete", makeInterceptFn("delete", cfg, db))
	db.Callback().Delete().After("gorm:commit_or_rollback_transaction").
		Register("gormspan:clear_intercept_delete", clearInterceptFn)

	log.Info().
		Int("tables", len(cfg.InterceptTables)).
		Msg("gormspan: write interception callbacks registered")
}

// makeInterceptFn returns a GORM callback that intercepts writes on
// target tables, serializes the row as a ManifestChange, and POSTs it
// to the configured Endpoint.
//
// For create operations, the full row data is sent as-is.
//
// For update operations, the current row is read from the database,
// the new values are merged on top, and the full merged row is sent
// as if it were an insert. This ensures the receiving end always gets
// the complete row state.
//
// For delete operations, the current row is read, serialized as JSON
// into the "row" field, and sent as a tombstone to "{table}_tombstone".
//
// rootDB is the root *gorm.DB used for read-back queries. Read paths
// are clean — no interception, no locks.
func makeInterceptFn(op string, cfg Config, rootDB *gorm.DB) func(*gorm.DB) {
	return func(tx *gorm.DB) {
		// Don't interfere if there's already an error.
		if tx.Error != nil {
			return
		}

		// Bypass: the business-logic service is writing directly.
		if isDirectWrite(tx.Statement.Context) {
			return
		}

		// Only intercept target tables.
		table := tx.Statement.Table
		if !cfg.InterceptTables[table] {
			return
		}

		now := time.Now().UTC()
		nowStr := now.Format(time.RFC3339Nano)

		switch op {
		case "create":
			records := extractRecords(tx)

			for _, record := range records {
				record["_created"] = nowStr
				record["_updated"] = nowStr

				mc := ManifestChange{
					DB:    cfg.DBName,
					Table: table,
					Data:  record,
				}

				if err := postManifestChange(cfg.Endpoint, mc); err != nil {
					_ = tx.AddError(err)
					return
				}
			}

			log.Trace().
				Str("op", op).
				Str("table", table).
				Int("records", len(records)).
				Msg("gormspan: create intercepted")

			tx.RowsAffected = int64(len(records))

		case "update":
			// For updates we send the full NEW row state.
			// 1. Extract the partial changes from the GORM statement.
			// 2. Determine the PK of the row being updated.
			// 3. Read the current full row from the DB.
			// 4. Merge the changes on top → full new row.
			// 5. POST as if it were an insert.
			partials := extractRecords(tx)

			for _, partial := range partials {
				pkValue := partial["id"]
				if pkValue == nil {
					log.Warn().
						Str("table", table).
						Msg("gormspan: update has no PK, skipping")
					continue
				}

				// Read current row from DB (read path is clean).
				fullRow := readCurrentRow(rootDB, table, pkValue)

				// Merge partial changes on top of the full row.
				if fullRow != nil {
					for k, v := range partial {
						fullRow[k] = v
					}
				} else {
					// No existing row (race or new row via Updates/Save).
					// Send what we have.
					fullRow = partial
				}

				fullRow["_updated"] = nowStr

				mc := ManifestChange{
					DB:    cfg.DBName,
					Table: table,
					Data:  fullRow,
				}

				if err := postManifestChange(cfg.Endpoint, mc); err != nil {
					_ = tx.AddError(err)
					return
				}
			}

			log.Trace().
				Str("op", op).
				Str("table", table).
				Int("records", len(partials)).
				Msg("gormspan: update intercepted")

			tx.RowsAffected = int64(len(partials))

		case "delete":
			// Read the full row, then send a tombstone with the row JSON.
			pkValue := extractDeletePK(tx)

			fullRow := readCurrentRow(rootDB, table, pkValue)

			// Serialize the full row as JSON for the "row" column.
			var rowJSON string
			if fullRow != nil {
				b, err := json.Marshal(fullRow)
				if err != nil {
					rowJSON = "{}"
				} else {
					rowJSON = string(b)
				}
			} else {
				rowJSON = "{}"
			}

			mc := ManifestChange{
				DB:    cfg.DBName,
				Table: table + "_tombstones",
				Data: map[string]any{
					"id":       pkValue,
					"row":      rowJSON,
					"_created": nowStr,
				},
			}

			if err := postManifestChange(cfg.Endpoint, mc); err != nil {
				_ = tx.AddError(err)
				return
			}

			log.Trace().
				Str("op", op).
				Str("table", table).
				Interface("pk", pkValue).
				Msg("gormspan: delete intercepted")

			tx.RowsAffected = 1
		}

		// Suppress the database write.
		tx.Error = errIntercepted
	}
}

// readCurrentRow reads the full current row from the database by PK.
// Uses a direct SQL query to get a map of all column values, avoiding
// any model/schema dependency. The read path is clean — no interception.
func readCurrentRow(db *gorm.DB, table string, pkValue any) map[string]any {
	if pkValue == nil {
		return nil
	}

	//nolint:perfsprint // table name is safe (from InterceptTables keys)
	query := fmt.Sprintf(`SELECT * FROM "%s" WHERE id = ? LIMIT 1`, table)

	rows, err := db.Raw(query, pkValue).Rows()
	if err != nil {
		log.Warn().Err(err).Str("table", table).Msg("gormspan: failed to read current row")
		return nil
	}
	defer rows.Close()

	if !rows.Next() {
		return nil
	}

	colNames, err := rows.Columns()
	if err != nil {
		return nil
	}

	values := make([]any, len(colNames))
	valuePtrs := make([]any, len(colNames))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	if err := rows.Scan(valuePtrs...); err != nil {
		log.Warn().Err(err).Str("table", table).Msg("gormspan: failed to scan current row")
		return nil
	}

	result := make(map[string]any, len(colNames))
	for i, colName := range colNames {
		v := values[i]
		// SQLite returns []byte for TEXT columns; convert to string.
		if b, ok := v.([]byte); ok {
			result[colName] = string(b)
		} else {
			result[colName] = v
		}
	}

	return result
}

// extractDeletePK extracts the primary key value from a GORM delete
// statement. GORM's Delete() can be called in several ways:
//
//	tx.Delete(&Node{}, nodeID)          → Clauses WHERE Eq{PrimaryColumn, nodeID}
//	tx.Delete(&Node{ID: nodeID})        → Vars = [], PK on struct
//	tx.Delete(&PreAuthKey{ID: keyID})   → Vars = [], PK on struct
//	tx.Where("id = ?", id).Delete(...)  → Vars = [id] (from Where)
//
// We check Vars first, then the Dest struct, then WHERE clauses.
func extractDeletePK(tx *gorm.DB) any {

	// 1. Check positional args from .Where("id = ?", id).
	if len(tx.Statement.Vars) > 0 {
		return tx.Statement.Vars[0]
	}

	// 2. Read PK from the Dest struct via schema introspection.
	if tx.Statement.Schema != nil {
		rv := tx.Statement.ReflectValue
		for rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}

		for _, field := range tx.Statement.Schema.PrimaryFields {
			val, isZero := field.ValueOf(tx.Statement.Context, rv)
			if !isZero {
				return val
			}
		}
	}

	// 3. Check WHERE clauses for primary key conditions.
	// Handles .Delete(&Model{}, id) where GORM's BuildCondition
	// puts id into clause.Eq{Column: PrimaryColumn, Value: id}.
	if whereClause, ok := tx.Statement.Clauses["WHERE"]; ok {
		if where, ok := whereClause.Expression.(clause.Where); ok {
			for _, expr := range where.Exprs {
				if eq, ok := expr.(clause.Eq); ok {
					// Check if this is the primary key sentinel.
					if col, ok := eq.Column.(clause.Column); ok && col.Name == clause.PrimaryKey {
						return eq.Value
					}
					// Also accept explicit "id" column.
					if col, ok := eq.Column.(clause.Column); ok && col.Name == "id" {
						return eq.Value
					}
				}
			}
		}
	}

	log.Warn().
		Str("table", tx.Statement.Table).
		Msg("gormspan: could not extract PK from delete statement")

	return nil
}

// postManifestChange serializes the ManifestChange as JSON and POSTs it
// to the given endpoint URL.
func postManifestChange(endpoint string, mc ManifestChange) error {
	if endpoint == "" {
		return fmt.Errorf("gormspan: no endpoint configured (set GORMSPAN_ENDPOINT)")
	}

	payload, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("gormspan: marshal ManifestChange: %w", err)
	}

	resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("gormspan: POST to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gormspan: POST to %s returned HTTP %d", endpoint, resp.StatusCode)
	}

	return nil
}

// clearInterceptFn runs after the core gorm:create/update/delete
// callback. If the error is our sentinel, it clears it so the caller
// sees a clean success.
func clearInterceptFn(tx *gorm.DB) {
	if errors.Is(tx.Error, errIntercepted) {
		tx.Error = nil
	}
}

// ---------------------------------------------------------------------------
// Model data extraction
// ---------------------------------------------------------------------------

// extractRecords builds a []map[column→value] from the GORM statement.
// Handles:
//   - Struct models (Create, Save, Updates with struct) → full field extraction
//   - Slices of structs (batch Create) → per-element extraction
//   - Maps (single-column .Update("col", val) or .Updates(map[...])) → direct map
//
// For map-based updates (partial column changes), GORM's ReflectValue
// is a map, not a struct, so we cannot use schema introspection. We
// read the map directly and inject the primary key from Statement.Vars
// so the receiving end knows which row was affected.
func extractRecords(tx *gorm.DB) []map[string]any {
	if tx.Statement == nil || tx.Statement.Schema == nil {
		return nil
	}

	rv := tx.Statement.ReflectValue
	switch rv.Kind() {
	case reflect.Map:
		// Partial update: .Update("col", val) or .Updates(map[string]any{...})
		record := make(map[string]any)
		for _, key := range rv.MapKeys() {
			record[fmt.Sprint(key.Interface())] = rv.MapIndex(key).Interface()
		}

		// Inject the primary key from Where conditions so the
		// receiving end knows which row is being updated.
		injectPKFromVars(tx, record)

		return []map[string]any{record}

	case reflect.Slice, reflect.Array:
		records := make([]map[string]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			records = append(records, extractFields(tx.Statement, rv.Index(i)))
		}
		return records

	default:
		// Struct model (Create, Save, Updates with struct).
		return []map[string]any{extractFields(tx.Statement, rv)}
	}
}

// injectPKFromVars tries to add the primary key to a partial-update
// record. For calls like:
//
//	tx.Model(&Node{}).Where("id = ?", nodeID).Update("col", val)
//
// The PK lives in Statement.Vars (from the Where clause).
// For calls like:
//
//	tx.Model(node).Update("col", val)    // node has ID set
//
// The PK lives on the Model struct's primary key field.
func injectPKFromVars(tx *gorm.DB, record map[string]any) {
	// Already has an "id" key — nothing to do.
	if _, ok := record["id"]; ok {
		return
	}

	// 1. Try the Model's primary key field (e.g. tx.Model(node) where
	//    node.ID is set).
	if tx.Statement.Model != nil && tx.Statement.Schema != nil {
		modelRV := reflect.ValueOf(tx.Statement.Model)
		for modelRV.Kind() == reflect.Ptr {
			modelRV = modelRV.Elem()
		}
		if modelRV.Kind() == reflect.Struct {
			for _, field := range tx.Statement.Schema.PrimaryFields {
				val, isZero := field.ValueOf(tx.Statement.Context, modelRV)
				if !isZero {
					record[field.DBName] = val
					return
				}
			}
		}
	}

	// 2. Fall back to Statement.Vars (from .Where("id = ?", id)).
	// Convention: the first var that looks like an integer PK.
	if len(tx.Statement.Vars) > 0 {
		record["id"] = tx.Statement.Vars[0]
	}
}

// extractFields reads every field defined in the GORM schema from the
// given reflect.Value and returns a column-name → value map.
//
// Fields that use a GORM serializer (serializer:json, serializer:text)
// are converted to their database representation via the serializer's
// Value method. This avoids JSON-marshal cycles caused by complex Go
// types (key.MachinePublic, tailcfg.Hostinfo, etc.) whose internal
// structure may reference *schema.Field.
//
// After extraction, values are normalized for JSON-flat serialization:
//   - sql.Null* wrappers are flattened (Valid=false → nil, Valid=true → inner value)
//   - time.Time values are forced to UTC so timestamps always carry a timezone
func extractFields(stmt *gorm.Statement, rv reflect.Value) map[string]any {
	data := make(map[string]any)
	for _, field := range stmt.Schema.Fields {
		// Skip pseudo-fields that don't map to a database column.
		if field.DBName == "" {
			continue
		}

		val, _ := field.ValueOf(stmt.Context, rv)

		// For serializer-backed fields, convert to the DB
		// representation (string / []byte) so the payload stays
		// JSON-safe and cycle-free.
		if field.Serializer != nil {
			dbVal, err := field.Serializer.Value(stmt.Context, field, rv, val)
			if err == nil {
				data[field.DBName] = dbVal
				continue
			}
			// Fallback: use fmt.Sprint so we never produce
			// an unmarshalable value.
			data[field.DBName] = fmt.Sprint(val)
			continue
		}

		data[field.DBName] = val
	}

	normalizeValues(data)
	return data
}

// normalizeValues makes a map JSON-flat and Python-friendly in place.
//
//  1. sql.Null* wrappers (sql.NullString, sql.NullInt64, …) are
//     flattened: if Valid is false the key becomes nil (JSON null);
//     if Valid is true the inner scalar is unwrapped. Detection is
//     generic via the driver.Valuer interface that all sql.Null*
//     types implement.
//
//  2. time.Time values are forced to UTC so every timestamp carries
//     an explicit timezone indicator (the Z suffix in RFC 3339).
//     Python's datetime.fromisoformat() then returns an offset-aware
//     datetime, preventing comparison TypeErrors on the receiving end.
func normalizeValues(data map[string]any) {
	for k, v := range data {
		if v == nil {
			continue
		}

		// --- time.Time → .UTC() ---
		switch tv := v.(type) {
		case time.Time:
			data[k] = tv.UTC()
			continue
		case *time.Time:
			if tv != nil {
				utc := tv.UTC()
				data[k] = &utc
			}
			continue
		}

		// --- sql.Null* → flatten via driver.Valuer ---
		// All sql.Null* types implement driver.Valuer. Valuer.Value()
		// returns (nil, nil) when Valid=false and (innerVal, nil) when
		// Valid=true — exactly the flattening we need.
		//
		// We also need the pointer form: field.ValueOf can return the
		// struct directly *or* behind a pointer, and reflect wraps it
		// in an interface. Check both the value and its addressable
		// (pointer) form.
		if flat, ok := flattenNullable(v); ok {
			data[k] = flat
			continue
		}
	}
}

// flattenNullable checks whether v implements driver.Valuer (the
// interface all sql.Null* types satisfy). If so it calls Value() to
// flatten: Valid=false → nil, Valid=true → inner scalar.
// Returns the flattened value and true, or (nil, false) if v is not
// a Valuer.
func flattenNullable(v any) (any, bool) {
	// Try v directly.
	if valuer, ok := v.(driver.Valuer); ok {
		dbVal, _ := valuer.Value()
		return dbVal, true
	}

	// Try pointer-to-v (sql.Null* methods have value receivers, but
	// the concrete value may be wrapped in an interface without being
	// addressable).
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Struct {
		ptr := reflect.New(rv.Type())
		ptr.Elem().Set(rv)
		if valuer, ok := ptr.Interface().(driver.Valuer); ok {
			dbVal, _ := valuer.Value()
			return dbVal, true
		}
	}

	return nil, false
}
