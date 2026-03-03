// Package gormspan is a GORM plugin that adds CRDT-friendly features to
// headscale's database layer without modifying any upstream files.
//
// Features:
//   - Random uint64 primary key generation (BeforeCreate hooks)
//   - SQLite verify(message, signature_b64) function — Ed25519 signature check
//   - SQLite notify(message, url) function — HTTP POST webhook
//   - Write interception: conditionally redirect writes on target tables
//     to an HTTP endpoint instead of the database. The receiving service
//     evaluates business logic and manifests results back via
//     [WithDirectWrite].
//   - Shadow columns (_created, _updated) added to target tables and
//     included in intercepted payloads — invisible to application models.
//
// Usage:
//
//	cfg := gormspan.Config{
//	    VerifyPublicKey: "base64-encoded-ed25519-public-key",
//	    NotifyURL:       "http://localhost:8080/webhook",
//	    NotifyEnabled:   true,
//	    InterceptEnabled: true,
//	    InterceptTables:  map[string]bool{"users": true, "nodes": true},
//	}
//
//	// Register SQLite functions BEFORE opening the database so the
//	// first connection already has verify() and notify() available
//	// (required if triggers reference them during migrations).
//	gormspan.RegisterSQLiteFunctions(cfg)
//
//	db, _ := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
//	db.Use(gormspan.New(cfg)) // registers random-ID + intercept hooks
//
// Bypassing interception (from the business-logic service):
//
//	ctx := gormspan.WithDirectWrite(context.Background())
//	db.WithContext(ctx).Create(&record) // hits DB directly
package gormspan

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

const pluginName = "gormspan"

// Config configures the gormspan GORM plugin.
type Config struct {
	// VerifyPublicKey is the base64url-encoded Ed25519 public key used
	// by the verify() SQL function. If empty, verify() always returns 0.
	VerifyPublicKey string

	// NotifyURL is the default HTTP endpoint for the notify() SQL
	// function AND for write-interception payloads. Individual SQL-level
	// calls can override it via the second argument. If empty, notify()
	// is still registered but will fail at call time unless a URL is
	// provided as the second argument, and write interception will error
	// on every intercepted write.
	NotifyURL string

	// NotifyEnabled controls whether notify() actually sends HTTP
	// requests. When false, notify() is registered but returns 1 as a
	// no-op. Useful for testing / dev environments.
	NotifyEnabled bool

	// InterceptTables maps database table names to true for tables
	// whose writes (CREATE, UPDATE, DELETE) should be intercepted,
	// serialized as JSON, and sent to NotifyURL instead of being
	// written to the database. Non-target tables are unaffected.
	//
	// Example: map[string]bool{"users": true, "nodes": true}
	InterceptTables map[string]bool

	// InterceptEnabled globally enables write interception. When false,
	// all writes go directly to the database regardless of
	// InterceptTables. Useful for disabling interception in dev/test.
	InterceptEnabled bool
}

// Plugin implements gorm.Plugin.
type Plugin struct {
	cfg Config
}

// New creates a new gormspan GORM plugin.
func New(cfg Config) *Plugin {
	return &Plugin{cfg: cfg}
}

// Name returns the plugin name (required by gorm.Plugin).
func (p *Plugin) Name() string {
	return pluginName
}

// Initialize is called by GORM when the plugin is registered via db.Use().
// It registers BeforeCreate callbacks for random ID generation, SQLite
// custom functions, shadow columns on target tables, and write
// interception callbacks.
func (p *Plugin) Initialize(db *gorm.DB) error {
	// Register random-ID BeforeCreate callbacks.
	registerIDCallbacks(db)

	log.Info().Msg("gormspan: random ID callbacks registered")

	// Register SQLite custom functions (verify, notify).
	if err := p.registerSQLiteFunctions(db); err != nil {
		return fmt.Errorf("gormspan: registering sqlite functions: %w", err)
	}

	// Ensure shadow columns (_created, _updated) exist on target tables.
	if p.cfg.InterceptEnabled && len(p.cfg.InterceptTables) > 0 {
		if err := ensureShadowColumns(db, p.cfg.InterceptTables); err != nil {
			return fmt.Errorf("gormspan: ensuring shadow columns: %w", err)
		}
	}

	// Register write-interception callbacks (create, update, delete).
	registerInterceptCallbacks(db, p.cfg)

	return nil
}
