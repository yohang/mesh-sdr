package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/pressly/goose/v3"
)

// versionTable is goose's version table.
const versionTable = "goose_db_version"

var (
	// ErrMigrationsPending means the binary has migrations the database lacks.
	ErrMigrationsPending = errors.New("migrations_pending")
	// ErrSchemaTooNew means the database was migrated by a newer binary.
	ErrSchemaTooNew = errors.New("schema_too_new")
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

// Migrator applies the embedded goose migrations, each in its own
// transaction on the writer connection.
type Migrator struct {
	reader   *sql.DB
	provider *goose.Provider
}

func newMigrator(writer, reader *sql.DB, fsys fs.FS) (*Migrator, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, writer, fsys,
		goose.WithDisableGlobalRegistry(true),
		goose.WithTableName(versionTable),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: migrations: %w", err)
	}

	return &Migrator{reader: reader, provider: p}, nil
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) ([]MigrationResult, error) {
	results, err := m.provider.Up(ctx)

	out := make([]MigrationResult, 0, len(results))
	for _, r := range results {
		out = append(out, MigrationResult{Version: r.Source.Version, Name: path.Base(r.Source.Path), Duration: r.Duration})
	}

	if err != nil {
		return out, fmt.Errorf("sqlite: migrate up: %w", err)
	}

	return out, nil
}

// Down rolls back the latest migration (development only).
func (m *Migrator) Down(ctx context.Context) (MigrationResult, error) {
	r, err := m.provider.Down(ctx)
	if errors.Is(err, goose.ErrNoNextVersion) {
		return MigrationResult{}, ErrNoMigration
	}

	if err != nil {
		return MigrationResult{}, fmt.Errorf("sqlite: migrate down: %w", err)
	}

	return MigrationResult{Version: r.Source.Version, Name: path.Base(r.Source.Path), Duration: r.Duration}, nil
}

// Status lists the migrations known to this binary and their state. Like
// Check, it only reads through the read-only pool: unlike goose's Status,
// it never creates the version table.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	appliedAt := map[int64]time.Time{}

	exists, err := m.versionTableExists(ctx)
	if err != nil {
		return nil, err
	}

	if exists {
		appliedAt, err = m.applied(ctx)
		if err != nil {
			return nil, err
		}
	}

	sources := m.provider.ListSources()
	out := make([]MigrationStatus, 0, len(sources))

	for _, src := range sources {
		at, applied := appliedAt[src.Version]
		out = append(out, MigrationStatus{Version: src.Version, Name: path.Base(src.Path), Applied: applied, AppliedAt: at})
	}

	return out, nil
}

// Check returns nil when the schema matches the binary exactly, and
// ErrMigrationsPending or ErrSchemaTooNew (in a *VersionError, or naming an
// applied migration the binary does not know) otherwise. It never writes.
func (m *Migrator) Check(ctx context.Context) error {
	var binary int64

	known := map[int64]bool{}

	for _, s := range m.provider.ListSources() {
		known[s.Version] = true
		binary = max(binary, s.Version)
	}

	exists, err := m.versionTableExists(ctx)
	if err != nil {
		return err
	}

	applied := map[int64]time.Time{}

	if exists {
		if applied, err = m.applied(ctx); err != nil {
			return err
		}
	}

	var current int64
	for v := range applied {
		current = max(current, v)
	}

	if current > binary {
		return &VersionError{Err: ErrSchemaTooNew, Database: current, Binary: binary}
	}

	for v := range applied {
		if !known[v] {
			return fmt.Errorf("%w: applied migration %d is unknown to this binary", ErrSchemaTooNew, v)
		}
	}

	if len(applied) < len(known) {
		return &VersionError{Err: ErrMigrationsPending, Database: current, Binary: binary}
	}

	return nil
}

func (m *Migrator) versionTableExists(ctx context.Context) (bool, error) {
	var n int
	if err := m.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", versionTable).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlite: look up table %s: %w", versionTable, err)
	}

	return n > 0, nil
}

// applied returns the applied versions (version 0 is goose's own bootstrap
// row) and when each was applied.
func (m *Migrator) applied(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := m.reader.QueryContext(ctx,
		"SELECT version_id, MAX(tstamp) FROM "+versionTable+" WHERE version_id > 0 AND is_applied GROUP BY version_id")
	if err != nil {
		return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
	}

	defer func() { _ = rows.Close() }()

	out := map[int64]time.Time{}

	for rows.Next() {
		var (
			v  int64
			ts any
		)

		if err := rows.Scan(&v, &ts); err != nil {
			return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
		}

		switch t := ts.(type) {
		case time.Time:
			out[v] = t.UTC()
		case string:
			parsed, err := time.Parse(time.DateTime, t)
			if err != nil {
				return nil, fmt.Errorf("sqlite: applied time of migration %d: %w", v, err)
			}

			out[v] = parsed
		default:
			out[v] = time.Time{}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
	}

	return out, nil
}
