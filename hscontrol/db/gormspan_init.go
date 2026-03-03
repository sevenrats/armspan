package db

// gormspan_init.go wires the gormspan GORM plugin into the database layer.
//
// This file is specific to the armspan fork. It is a NEW file (not
// present upstream) so it never causes rebase conflicts.
//
// The only upstream touch is a single line in db.go:
//
//	postOpenDBHook(dbConn)
//
// That line calls the hook variable defined here. If this file were
// ever removed, adding a stub "var postOpenDBHook = func(*gorm.DB){}" 
// in any file in this package restores compilation.
//
// Configuration is via environment variables:
//
//	GORMSPAN_ENDPOINT — URL to POST ManifestChange payloads to
//	GORMSPAN_DB       — logical database name for payloads

import (
	"github.com/juanfont/headscale/gormspan"
	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// postOpenDBHook is called from NewHeadscaleDatabase right after
// openDB() returns. It is a variable so that gormspan_init.go (this
// file) can set it via init() without modifying db.go beyond one line.
var postOpenDBHook = func(db *gorm.DB) {}

func init() {
	postOpenDBHook = func(db *gorm.DB) {
		cfg := gormspan.ConfigFromEnv()

		// Target tables whose writes should be intercepted.
		cfg.InterceptTables = map[string]bool{
			"users":         true,
			"nodes":         true,
			"pre_auth_keys": true,
			"api_keys":      true,
			"routes":        true,
			"policies":      true,
		}

		plugin := gormspan.New(cfg)
		if err := db.Use(plugin); err != nil {
			log.Fatal().Err(err).Msg("gormspan: failed to register GORM plugin")
		}
	}
}
