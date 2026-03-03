// Package gormspan is a GORM plugin that adds CRDT-friendly features to
// headscale's database layer without modifying any upstream files.
//
// Features:
//   - Random uint64 primary key generation (BeforeCreate hooks)
//   - Shadow columns (_created, _updated) added to target tables —
//     invisible to application models.
//   - Write interception: all CRUD on target tables is redirected as an
//     HTTP POST to a configurable endpoint. DELETE operations are
//     converted into tombstone insertions.
//
// The POST payload matches the Python DTO:
//
//	@dataclass
//	class ManifestChange(DataClass):
//	    db: str
//	    table: str
//	    data: dict
//
// Configuration is via environment variables:
//
//	GORMSPAN_ENDPOINT   — URL to POST ManifestChange payloads to
//	GORMSPAN_DB         — database name included in every payload
//
// Usage:
//
//	cfg := gormspan.ConfigFromEnv()
//	cfg.InterceptTables = map[string]bool{"users": true, "nodes": true}
//
//	db, _ := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
//	db.Use(gormspan.New(cfg))
//
// Bypassing interception (from the business-logic service):
//
//	ctx := gormspan.WithDirectWrite(context.Background())
//	db.WithContext(ctx).Create(&record) // hits DB directly
package gormspan

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

const pluginName = "gormspan"

// Config configures the gormspan GORM plugin.
type Config struct {
	// Endpoint is the URL to POST ManifestChange payloads to.
	// Typically read from GORMSPAN_ENDPOINT env var.
	Endpoint string

	// DBName is the logical database identifier included in every
	// ManifestChange payload. Typically read from GORMSPAN_DB env var.
	DBName string

	// InterceptTables maps database table names to true for tables
	// whose writes (CREATE, UPDATE, DELETE) should be intercepted and
	// sent to Endpoint. Non-target tables are unaffected.
	//
	// Example: map[string]bool{"users": true, "nodes": true}
	InterceptTables map[string]bool

	// InterceptEnabled globally enables write interception. When false,
	// all writes go directly to the database regardless of
	// InterceptTables.
	InterceptEnabled bool
}

// ConfigFromEnv builds a Config from environment variables.
//
//	GORMSPAN_ENDPOINT — URL to POST ManifestChange payloads to
//	GORMSPAN_DB       — logical database name for payloads
func ConfigFromEnv() Config {
	return Config{
		Endpoint:         os.Getenv("GORMSPAN_ENDPOINT"),
		DBName:           os.Getenv("GORMSPAN_DB"),
		InterceptEnabled: os.Getenv("GORMSPAN_ENDPOINT") != "",
	}
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
// It registers BeforeCreate callbacks for random ID generation, ensures
// shadow columns on target tables, and registers write interception
// callbacks.
func (p *Plugin) Initialize(db *gorm.DB) error {
	// Register random-ID BeforeCreate callbacks.
	registerIDCallbacks(db)
	log.Info().Msg("gormspan: random ID callbacks registered")

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
