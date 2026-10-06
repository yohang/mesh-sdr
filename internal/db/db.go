// Package db is the engine contract of the database adapter layer
// (TECHNICAL_SPEC §3.3, §7.2): every dialect adapter (internal/db/<dialect>)
// implements Adapter, and repositories (internal/<module>/infra/<dialect>)
// reach the database only through it. Domain and application code never
// import this package: they depend on repository interfaces.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Querier runs SQL. It is satisfied by *sql.DB, *sql.Conn and *sql.Tx, and
// matches the DBTX interface of sqlc-generated code.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Adapter is the engine contract implemented by every dialect adapter.
//
// Writes are serialised by the adapter (single writer). A unit of work runs
// in WithinTx; the transaction travels in the context, so repositories call
// Reader(ctx)/Writer(ctx) and transparently join it.
type Adapter interface {
	// Dialect returns the engine of this adapter.
	Dialect() Dialect
	// Reader returns the querier for reads: the current transaction when ctx
	// carries one, the read-only pool otherwise.
	Reader(ctx context.Context) Querier
	// Writer returns the querier for writes: the current transaction when ctx
	// carries one, the single writer connection otherwise.
	Writer(ctx context.Context) Querier
	// WithinTx runs fn in one write transaction, committed when fn returns
	// nil and rolled back otherwise. Nested calls join the outer transaction.
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
	// Migrator returns the schema migrator of this dialect.
	Migrator() Migrator
	// Ping checks that the database answers.
	Ping(ctx context.Context) error
	// Close releases every connection.
	Close() error
}

// Migrator applies the forward-only, per-dialect schema migrations.
type Migrator interface {
	// Up applies every pending migration, each in its own transaction.
	Up(ctx context.Context) ([]MigrationResult, error)
	// Down rolls back the latest migration (development only).
	Down(ctx context.Context) (MigrationResult, error)
	// Status lists the migrations known to this binary and their state.
	Status(ctx context.Context) ([]MigrationStatus, error)
	// Check returns nil when the schema matches the binary exactly. It
	// returns ErrMigrationsPending, ErrSchemaTooNew or ErrChecksumMismatch
	// (wrapped in a *VersionError or with the offending migration) and never
	// writes to the database.
	Check(ctx context.Context) error
}

// MigrationResult describes one applied or rolled-back migration.
type MigrationResult struct {
	Version  int64
	Name     string
	Duration time.Duration
}

// MigrationStatus describes one migration known to the binary.
type MigrationStatus struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

var (
	// ErrMigrationsPending means the binary has migrations the database lacks.
	ErrMigrationsPending = errors.New("migrations_pending")
	// ErrSchemaTooNew means the database was migrated by a newer binary.
	ErrSchemaTooNew = errors.New("schema_too_new")
	// ErrChecksumMismatch means an applied migration was modified, or its
	// checksum is missing.
	ErrChecksumMismatch = errors.New("migration_checksum_mismatch")
	// ErrNoMigration means there is no applied migration to roll back.
	ErrNoMigration = errors.New("no_migration")
)

// VersionError reports a schema version mismatch between the database and
// the binary. It matches ErrMigrationsPending or ErrSchemaTooNew with
// errors.Is.
type VersionError struct {
	Err      error
	Database int64 // schema version of the database
	Binary   int64 // latest schema version known to the binary
}

func (e *VersionError) Error() string {
	switch {
	case errors.Is(e.Err, ErrSchemaTooNew):
		return fmt.Sprintf("%v: database schema version %d is newer than this binary supports (%d); upgrade meshsdr", e.Err, e.Database, e.Binary)
	case errors.Is(e.Err, ErrMigrationsPending):
		return fmt.Sprintf("%v: database schema version %d, binary expects %d; run `meshsdr hub migrate`", e.Err, e.Database, e.Binary)
	default:
		return fmt.Sprintf("%v: database schema version %d, binary %d", e.Err, e.Database, e.Binary)
	}
}

func (e *VersionError) Unwrap() error { return e.Err }
