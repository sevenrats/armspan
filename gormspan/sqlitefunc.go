package gormspan

import (
	"database/sql/driver"

	sqlite "github.com/glebarez/go-sqlite"
	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// registerSQLiteFunctions registers verify() and notify() as custom
// scalar SQLite functions via the glebarez/go-sqlite driver.
//
// Functions are registered on the package-level driver singleton and
// will be automatically applied to every new connection the driver
// opens.  This means they MUST be registered BEFORE gorm.Open() is
// called so the first connection already has them (important when
// triggers reference these functions during migrations).
//
// For non-SQLite dialects this is a silent no-op.
func (p *Plugin) registerSQLiteFunctions(db *gorm.DB) error {
	// Quick dialect check — skip registration for postgres.
	if db.Dialector.Name() != "sqlite" {
		log.Debug().Msg("gormspan: not sqlite, skipping SQLite function registration")
		return nil
	}

	p.doRegisterSQLiteFunctions()
	return nil
}

func (p *Plugin) doRegisterSQLiteFunctions() {
	// ── verify(message, signature_b64) → int ──
	verifyFn := verifyFunc(p.cfg.VerifyPublicKey)
	if err := sqlite.RegisterDeterministicScalarFunction(
		"verify", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			msg, _ := args[0].(string)
			sig, _ := args[1].(string)
			return verifyFn(msg, sig), nil
		},
	); err != nil {
		log.Warn().Err(err).Msg("gormspan: failed to register verify() (may already be registered)")
	} else {
		log.Info().Msg("gormspan: verify() SQL function registered")
	}

	// ── notify(message, url) → int ──
	notifyFn := notifyFunc(p.cfg.NotifyURL, p.cfg.NotifyEnabled)
	if err := sqlite.RegisterScalarFunction(
		"notify", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			msg, _ := args[0].(string)
			url, _ := args[1].(string)
			return notifyFn(msg, url), nil
		},
	); err != nil {
		log.Warn().Err(err).Msg("gormspan: failed to register notify() (may already be registered)")
	} else {
		log.Info().Msg("gormspan: notify() SQL function registered")
	}
}

// RegisterSQLiteFunctions registers the verify() and notify() custom
// SQLite functions on the glebarez/go-sqlite driver.  Call this BEFORE
// gorm.Open() to ensure the first connection already has the functions.
//
// This is also called automatically by Plugin.Initialize(), but if
// triggers depend on these functions during migrations, you must call
// this earlier.
//
//	gormspan.RegisterSQLiteFunctions(cfg)   // before DB open
//	db, _ := gorm.Open(sqlite.Open(dsn))   // connection has functions
//	db.Use(gormspan.New(cfg))               // registers ID callbacks
func RegisterSQLiteFunctions(cfg Config) {
	p := &Plugin{cfg: cfg}
	p.doRegisterSQLiteFunctions()
}
