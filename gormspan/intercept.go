package gormspan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
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
// and injects the sentinel error. The "after" callback clears the
// sentinel so the caller sees a clean success.
func registerInterceptCallbacks(db *gorm.DB, cfg Config) {
	if !cfg.InterceptEnabled || len(cfg.InterceptTables) == 0 {
		return
	}

	// ── Create ──
	db.Callback().Create().Before("gorm:create").
		Register("gormspan:intercept_create", makeInterceptFn("create", cfg))
	db.Callback().Create().After("gorm:create").Before("gorm:after_create").
		Register("gormspan:clear_intercept_create", clearInterceptFn)

	// ── Update ──
	db.Callback().Update().Before("gorm:update").
		Register("gormspan:intercept_update", makeInterceptFn("update", cfg))
	db.Callback().Update().After("gorm:update").Before("gorm:after_update").
		Register("gormspan:clear_intercept_update", clearInterceptFn)

	// ── Delete ──
	db.Callback().Delete().Before("gorm:delete").
		Register("gormspan:intercept_delete", makeInterceptFn("delete", cfg))
	db.Callback().Delete().After("gorm:delete").Before("gorm:after_delete").
		Register("gormspan:clear_intercept_delete", clearInterceptFn)

	log.Info().
		Int("tables", len(cfg.InterceptTables)).
		Msg("gormspan: write interception callbacks registered")
}

// makeInterceptFn returns a GORM callback that intercepts writes on
// target tables, serializes the row as a ManifestChange, and POSTs it
// to the configured Endpoint.
//
// For create/update operations, the ManifestChange uses the original
// table name and the row data (with shadow columns injected).
//
// For delete operations, the write is converted into a tombstone
// insertion: the ManifestChange uses "{table}_tombstone" and the data
// contains {id: pk_value, row: json(original_row), _created: now}.
func makeInterceptFn(op string, cfg Config) func(*gorm.DB) {
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

		// Extract model data from the GORM statement.
		records := extractRecords(tx)

		for _, record := range records {
			var mc ManifestChange

			switch op {
			case "create":
				record["_created"] = nowStr
				record["_updated"] = nowStr
				mc = ManifestChange{
					DB:    cfg.DBName,
					Table: table,
					Data:  record,
				}

			case "update":
				record["_updated"] = nowStr
				mc = ManifestChange{
					DB:    cfg.DBName,
					Table: table,
					Data:  record,
				}

			case "delete":
				mc = buildTombstone(cfg.DBName, table, record, nowStr)
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
			Msg("gormspan: write intercepted")

		// Suppress the database write.
		tx.RowsAffected = int64(len(records))
		tx.Error = errIntercepted
	}
}

// buildTombstone converts a delete operation into a tombstone
// ManifestChange. The tombstone table is "{table}_tombstone" and
// contains:
//
//	id       — the primary key value from the original row
//	row      — full JSON serialization of the original row
//	_created — timestamp of the deletion
func buildTombstone(dbName, table string, rowData map[string]any, nowStr string) ManifestChange {
	// Find the primary key value (prefer "id" column).
	pkValue := rowData["id"]

	// Serialize the full row as JSON for the "row" column.
	rowJSON, err := json.Marshal(rowData)
	if err != nil {
		rowJSON = []byte("{}")
	}

	return ManifestChange{
		DB:    dbName,
		Table: table + "_tombstone",
		Data: map[string]any{
			"id":       pkValue,
			"row":      string(rowJSON),
			"_created": nowStr,
		},
	}
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
// Handles both single-record and batch (slice) operations.
func extractRecords(tx *gorm.DB) []map[string]any {
	if tx.Statement == nil || tx.Statement.Schema == nil {
		return nil
	}

	rv := tx.Statement.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		records := make([]map[string]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			records = append(records, extractFields(tx.Statement, rv.Index(i)))
		}
		return records
	default:
		return []map[string]any{extractFields(tx.Statement, rv)}
	}
}

// extractFields reads every field defined in the GORM schema from the
// given reflect.Value and returns a column-name → value map.
func extractFields(stmt *gorm.Statement, rv reflect.Value) map[string]any {
	data := make(map[string]any)
	for _, field := range stmt.Schema.Fields {
		val, _ := field.ValueOf(stmt.Context, rv)
		data[field.DBName] = val
	}
	return data
}
