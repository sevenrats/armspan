package db

// gormspan_init.go wires the gormspan GORM plugin into the database layer.
//
// This file is specific to the armspan fork. It is a NEW file (not
// present upstream) so it never causes rebase conflicts.
//
// The sync-release-tags.sh script injects a one-line call into db.go
// via sed when building release tags:
//
//	postOpenDBHook(dbConn)
//
// On main-armspan this file simply defines the function. The hook
// line is only present in versioned *-armspan tags.
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

// postOpenDBHook is injected into db.go by the tag-sync script.
// It registers the gormspan GORM plugin (random IDs, shadow columns,
// write interception) on the given database connection.
func postOpenDBHook(db *gorm.DB) {
	cfg := gormspan.ConfigFromEnv()

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
