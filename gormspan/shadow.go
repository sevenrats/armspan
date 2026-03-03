package gormspan

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// shadowColumns are the extra columns added to every target table.
// They are invisible to application model structs — headscale never
// needs to know about them. The interceptor populates them in the
// serialized payload and the external business-logic service persists
// them when it manifests writes via [WithDirectWrite].
var shadowColumns = []struct {
	name    string
	pgType  string // PostgreSQL column definition
	sqlType string // SQLite column definition
}{
	{"_created", "TIMESTAMPTZ", "TEXT"},
	{"_updated", "TIMESTAMPTZ", "TEXT"},
}

// ensureShadowColumns adds _created and _updated columns to every
// target table if they do not already exist. The columns are nullable
// with no default — existing rows will have NULL until backfilled.
//
// Supports PostgreSQL and SQLite dialects.
func ensureShadowColumns(db *gorm.DB, tables map[string]bool) error {
	dialect := db.Dialector.Name()

	for table := range tables {
		for _, col := range shadowColumns {
			switch dialect {
			case "postgres":
				if err := addColumnPostgres(db, table, col.name, col.pgType); err != nil {
					return err
				}
			case "sqlite":
				if err := addColumnSQLite(db, table, col.name, col.sqlType); err != nil {
					return err
				}
			default:
				return fmt.Errorf("gormspan: unsupported dialect %q for shadow columns", dialect)
			}
		}

		log.Info().Str("table", table).Msg("gormspan: shadow columns ensured")
	}

	return nil
}

// addColumnPostgres uses IF NOT EXISTS (PostgreSQL 9.6+).
func addColumnPostgres(db *gorm.DB, table, column, colType string) error {
	sql := fmt.Sprintf(
		`ALTER TABLE "%s" ADD COLUMN IF NOT EXISTS "%s" %s`,
		table, column, colType,
	)
	if err := db.Exec(sql).Error; err != nil {
		return fmt.Errorf("gormspan: adding column %s.%s: %w", table, column, err)
	}
	return nil
}

// addColumnSQLite tries to ADD COLUMN and silently ignores "duplicate
// column name" errors (SQLite does not support IF NOT EXISTS for
// ALTER TABLE ADD COLUMN).
func addColumnSQLite(db *gorm.DB, table, column, colType string) error {
	sql := fmt.Sprintf(
		`ALTER TABLE "%s" ADD COLUMN "%s" %s`,
		table, column, colType,
	)
	if err := db.Exec(sql).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			log.Debug().
				Str("table", table).
				Str("column", column).
				Msg("gormspan: shadow column already exists")
			return nil
		}
		return fmt.Errorf("gormspan: adding column %s.%s: %w", table, column, err)
	}
	return nil
}
