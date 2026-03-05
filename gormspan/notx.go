package gormspan

import (
	"context"
	"database/sql"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// noTxPool wraps a real gorm.ConnPool and turns all transaction
// machinery into no-ops. When gormspan is intercepting writes, we
// must NEVER let any code path acquire a SQLite write lock — the
// synchronous POST to the Python sync endpoint would deadlock
// because Python writes back into the same database.
//
// By replacing the ConnPool at plugin initialization time, every
// call to db.Begin(), db.Transaction(), tx.Commit(), tx.Rollback()
// throughout the entire codebase becomes a harmless no-op. This is
// fully generalized — no upstream code needs to be modified.
//
// Interfaces implemented:
//   - gorm.ConnPool         — delegates queries to the real pool
//   - gorm.ConnPoolBeginner — returns self (no real transaction)
//   - gorm.TxCommitter      — Commit/Rollback are no-ops
//   - gorm.GetDBConnector   — delegates so db.DB() still works
type noTxPool struct {
	inner gorm.ConnPool
}

// ---- gorm.ConnPool (delegates all query operations) ----

func (p *noTxPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return p.inner.PrepareContext(ctx, query)
}

func (p *noTxPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return p.inner.ExecContext(ctx, query, args...)
}

func (p *noTxPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.inner.QueryContext(ctx, query, args...)
}

func (p *noTxPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.inner.QueryRowContext(ctx, query, args...)
}

// ---- gorm.ConnPoolBeginner — returns self, no real transaction ----
//
// GORM's db.Begin() checks TxBeginner first (returns *sql.Tx),
// then ConnPoolBeginner (returns ConnPool). Since noTxPool does
// NOT implement TxBeginner, GORM falls through to ConnPoolBeginner
// and accepts our pool as the "transaction".

func (p *noTxPool) BeginTx(_ context.Context, _ *sql.TxOptions) (gorm.ConnPool, error) {
	return p, nil
}

// ---- gorm.TxCommitter — no-op commit/rollback ----
//
// GORM's Commit() and Rollback() check for TxCommitter on the
// ConnPool. Our no-ops satisfy the interface so callers see success.

func (p *noTxPool) Commit() error   { return nil }
func (p *noTxPool) Rollback() error { return nil }

// ---- gorm.GetDBConnector — so db.DB() keeps working ----
//
// Code that calls db.DB() (e.g. schema validation, Close()) needs
// to reach the real *sql.DB underneath. We delegate to the inner
// pool's GetDBConn if available, or try a direct type assertion.

func (p *noTxPool) GetDBConn() (*sql.DB, error) {
	type dbConnector interface {
		GetDBConn() (*sql.DB, error)
	}

	if c, ok := p.inner.(dbConnector); ok {
		return c.GetDBConn()
	}

	if sqlDB, ok := p.inner.(*sql.DB); ok {
		return sqlDB, nil
	}

	return nil, gorm.ErrInvalidDB
}

// installNoTxPool replaces the ConnPool on db with a noTxPool wrapper
// that makes all transaction operations (Begin, Commit, Rollback) into
// no-ops. It also disables GORM's nested transaction (savepoint) logic
// so that Transaction()-inside-Transaction() just passes through.
//
// This must be called once during plugin initialization, before any
// operations execute.
func installNoTxPool(db *gorm.DB) {
	pool := &noTxPool{inner: db.ConnPool}

	// Replace at Config level (used when GORM opens new Sessions).
	db.Config.ConnPool = pool

	// Replace on the initial Statement (cloned into every new *gorm.DB).
	db.Statement.ConnPool = pool

	// Disable savepoints for nested Transaction() calls. With our
	// no-op pool, GORM sees TxCommitter and thinks it's already in
	// a transaction. Without this flag it would issue SAVEPOINT SQL
	// which can still acquire locks.
	db.DisableNestedTransaction = true

	log.Info().Msg("gormspan: transaction pool replaced with no-op (all Begin/Commit/Rollback suppressed)")
}
