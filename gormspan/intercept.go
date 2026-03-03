package gormspan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// WriteEvent — the JSON payload sent to the notify endpoint
// ---------------------------------------------------------------------------

// WriteEvent is the JSON payload sent to NotifyURL when a write on a
// target table is intercepted. The receiving service evaluates business
// logic and ultimately writes the (potentially transformed) data to the
// database using [WithDirectWrite] to bypass re-interception.
type WriteEvent struct {
	// Op is one of "create", "update", or "delete".
	Op string `json:"op"`

	// Table is the database table name (e.g. "users").
	Table string `json:"table"`

	// Records contains one map per affected row. Keys are database
	// column names (from the GORM schema). Shadow columns (_created,
	// _updated) are included where applicable.
	Records []map[string]any `json:"records"`

	// Time is the UTC timestamp of the interception (RFC 3339 nano).
	Time string `json:"time"`
}

// ---------------------------------------------------------------------------
// Callback registration
// ---------------------------------------------------------------------------

// registerInterceptCallbacks sets up Before/After callbacks for
// gorm:create, gorm:update, and gorm:delete. The "before" callback
// serializes the write, notifies the endpoint, and injects the
// sentinel error. The "after" callback clears the sentinel so the
// caller sees a clean success.
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
// target tables, serializes the model data (with shadow columns), and
// POSTs it to the configured NotifyURL. The actual database write is
// suppressed by injecting errIntercepted.
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

		// Inject shadow columns.
		for i := range records {
			switch op {
			case "create":
				records[i]["_created"] = nowStr
				records[i]["_updated"] = nowStr
			case "update":
				records[i]["_updated"] = nowStr
			// delete: no shadow columns needed (event Time suffices)
			}
		}

		event := WriteEvent{
			Op:      op,
			Table:   table,
			Records: records,
			Time:    nowStr,
		}

		payload, err := json.Marshal(event)
		if err != nil {
			_ = tx.AddError(fmt.Errorf("gormspan: marshal write event: %w", err))
			return
		}

		// Send to the management service.
		url := cfg.NotifyURL
		if url == "" {
			_ = tx.AddError(fmt.Errorf("gormspan: no NotifyURL configured for write interception"))
			return
		}

		resp, err := notifyClient.Post(url, "application/json", bytes.NewReader(payload))
		if err != nil {
			_ = tx.AddError(fmt.Errorf("gormspan: intercept notify failed: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = tx.AddError(fmt.Errorf("gormspan: intercept notify returned HTTP %d", resp.StatusCode))
			return
		}

		log.Trace().
			Str("op", op).
			Str("table", table).
			Int("records", len(records)).
			Msg("gormspan: write intercepted and notified")

		// Suppress the database write.
		tx.RowsAffected = int64(len(records))
		tx.Error = errIntercepted
	}
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
