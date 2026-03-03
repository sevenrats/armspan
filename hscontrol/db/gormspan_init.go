package db

// gormspan_init.go wires the gormspan GORM plugin into the database layer.
//
// This file is specific to the armspan fork. It is a NEW file (not
// present upstream) so it never causes rebase conflicts.

import (
	"os"

	"github.com/juanfont/headscale/gormspan"
	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

func init() {
	// Register SQLite custom functions (verify, notify) on the
	// glebarez/go-sqlite driver at package load time — before any
	// database connection is opened.  Because sql.Open() is lazy,
	// this guarantees the first connection already has the functions
	// available (important if migrations create triggers that call them).
	cfg := gormspanConfigFromEnv()
	gormspan.RegisterSQLiteFunctions(cfg)
	log.Info().Msg("gormspan: SQLite functions registered on driver (init)")
}

// initGormspanPlugin registers the GORM callbacks (random ID generation)
// on the given database connection.  Call this after openDB() and
// before running migrations.
func initGormspanPlugin(db *gorm.DB) {
	cfg := gormspanConfigFromEnv()
	plugin := gormspan.New(cfg)
	if err := db.Use(plugin); err != nil {
		log.Fatal().Err(err).Msg("gormspan: failed to register GORM plugin")
	}
}

// gormspanConfigFromEnv builds a gormspan.Config from environment variables.
//
//	GORMSPAN_VERIFY_PUBLIC_KEY  — base64-encoded Ed25519 public key
//	GORMSPAN_NOTIFY_URL         — default webhook URL for notify()
//	GORMSPAN_NOTIFY_ENABLED     — "true" to enable HTTP POSTs (default: false)
func gormspanConfigFromEnv() gormspan.Config {
	return gormspan.Config{
		VerifyPublicKey: os.Getenv("GORMSPAN_VERIFY_PUBLIC_KEY"),
		NotifyURL:       os.Getenv("GORMSPAN_NOTIFY_URL"),
		NotifyEnabled:   os.Getenv("GORMSPAN_NOTIFY_ENABLED") == "true",
	}
}
