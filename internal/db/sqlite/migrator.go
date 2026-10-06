package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/yohang/mesh-sdr/internal/db"
)

const (
	// versionTable is goose's version table.
	versionTable = "goose_db_version"
	// checksumTable records the SHA-256 of every applied migration file
	// (TECHNICAL_SPEC §7.5: migrations are checksummed). goose has no
	// checksums, so the migrator keeps them next to goose's table.
	checksumTable = "schema_migration_checksums"
)

// Migrator implements db.Migrator with goose and per-migration checksums.
type Migrator struct {
	lockPath string
	writer   *sql.DB
	reader   *sql.DB
	fsys     fs.FS
	provider *goose.Provider
}

var _ db.Migrator = (*Migrator)(nil)

func newMigrator(dbPath string, writer, reader *sql.DB, fsys fs.FS) (*Migrator, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, writer, fsys,
		goose.WithDisableGlobalRegistry(true),
		goose.WithTableName(versionTable),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: migrations: %w", err)
	}

	return &Migrator{lockPath: dbPath + ".migrate.lock", writer: writer, reader: reader, fsys: fsys, provider: p}, nil
}

// locked runs fn while holding the exclusive migration lock
// (<db>.migrate.lock), so that two migration commands never interleave.
func (m *Migrator) locked(ctx context.Context, fn func() error) (err error) {
	l, err := lockFile(ctx, m.lockPath)
	if err != nil {
		return err
	}

	defer func() {
		if uerr := l.unlock(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("sqlite: release migration lock: %w", uerr))
		}
	}()

	return fn()
}

// Up implements db.Migrator. It refuses to run when an applied migration was
// modified, then applies the pending ones and records their checksums.
func (m *Migrator) Up(ctx context.Context) (out []db.MigrationResult, err error) {
	err = m.locked(ctx, func() error {
		out, err = m.up(ctx)

		return err
	})

	return out, err
}

func (m *Migrator) up(ctx context.Context) ([]db.MigrationResult, error) {
	if err := m.ensureChecksumTable(ctx); err != nil {
		return nil, err
	}

	if err := m.verifyChecksums(ctx, m.writer, false); err != nil {
		return nil, err
	}

	results, upErr := m.provider.Up(ctx)

	// Record the checksums of the migrations applied by this run (replacing
	// any stale value, e.g. after down → edit → up), and of applied
	// migrations that have none (applied before an error or by an older
	// binary).
	applied := make([]int64, 0, len(results))
	for _, r := range results {
		if r.Error == nil {
			applied = append(applied, r.Source.Version)
		}
	}

	if err := m.recordChecksums(ctx, applied); err != nil {
		return nil, errors.Join(upErr, err)
	}

	out := make([]db.MigrationResult, 0, len(results))
	for _, r := range results {
		out = append(out, db.MigrationResult{Version: r.Source.Version, Name: path.Base(r.Source.Path), Duration: r.Duration})
	}

	if upErr != nil {
		return out, fmt.Errorf("sqlite: migrate up: %w", upErr)
	}

	return out, nil
}

// Down implements db.Migrator.
func (m *Migrator) Down(ctx context.Context) (out db.MigrationResult, err error) {
	err = m.locked(ctx, func() error {
		out, err = m.down(ctx)

		return err
	})

	return out, err
}

// down rolls back the latest migration, then deletes its checksum on the
// single writer connection. goose runs the rollback in its own transaction,
// so the two cannot share one; a checksum left behind by a crash in between
// is replaced by the next Up, which upserts the checksums it applies.
func (m *Migrator) down(ctx context.Context) (db.MigrationResult, error) {
	r, err := m.provider.Down(ctx)
	if errors.Is(err, goose.ErrNoNextVersion) {
		return db.MigrationResult{}, db.ErrNoMigration
	}

	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("sqlite: migrate down: %w", err)
	}

	if err := m.ensureChecksumTable(ctx); err != nil {
		return db.MigrationResult{}, err
	}

	if _, err := m.writer.ExecContext(ctx, "DELETE FROM "+checksumTable+" WHERE version = ?", r.Source.Version); err != nil {
		return db.MigrationResult{}, fmt.Errorf("sqlite: delete checksum %d: %w", r.Source.Version, err)
	}

	return db.MigrationResult{Version: r.Source.Version, Name: path.Base(r.Source.Path), Duration: r.Duration}, nil
}

// Status implements db.Migrator. Like Check, it only reads through the
// read-only pool: unlike goose's Status, it never creates the version table.
func (m *Migrator) Status(ctx context.Context) ([]db.MigrationStatus, error) {
	appliedAt := map[int64]time.Time{}

	exists, err := tableExists(ctx, m.reader, versionTable)
	if err != nil {
		return nil, err
	}

	if exists {
		appliedAt, err = applyTimes(ctx, m.reader)
		if err != nil {
			return nil, err
		}
	}

	sources := m.provider.ListSources()
	out := make([]db.MigrationStatus, 0, len(sources))

	for _, src := range sources {
		at, applied := appliedAt[src.Version]
		out = append(out, db.MigrationStatus{
			Version:   src.Version,
			Name:      path.Base(src.Path),
			Applied:   applied,
			AppliedAt: at,
		})
	}

	return out, nil
}

// applyTimes returns when each applied version was applied.
func applyTimes(ctx context.Context, q *sql.DB) (map[int64]time.Time, error) {
	rows, err := q.QueryContext(ctx,
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

// Check implements db.Migrator. It only reads, through the read-only pool.
func (m *Migrator) Check(ctx context.Context) error {
	var binary int64

	known := map[int64]bool{}

	for _, s := range m.provider.ListSources() {
		known[s.Version] = true
		binary = max(binary, s.Version)
	}

	exists, err := tableExists(ctx, m.reader, versionTable)
	if err != nil {
		return err
	}

	var applied []int64

	if exists {
		applied, err = appliedVersions(ctx, m.reader)
		if err != nil {
			return err
		}
	}

	var current int64
	for _, v := range applied {
		current = max(current, v)
	}

	if current > binary {
		return &db.VersionError{Err: db.ErrSchemaTooNew, Database: current, Binary: binary}
	}

	for _, v := range applied {
		if !known[v] {
			return fmt.Errorf("%w: applied migration %d is unknown to this binary", db.ErrSchemaTooNew, v)
		}
	}

	if len(applied) < len(known) {
		return &db.VersionError{Err: db.ErrMigrationsPending, Database: current, Binary: binary}
	}

	return m.verifyChecksums(ctx, m.reader, true)
}

func tableExists(ctx context.Context, q *sql.DB, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlite: look up table %s: %w", name, err)
	}

	return n > 0, nil
}

// appliedVersions lists the versions recorded by goose (version 0 is goose's
// own bootstrap row).
func appliedVersions(ctx context.Context, q *sql.DB) ([]int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT version_id FROM "+versionTable+" WHERE version_id > 0 AND is_applied ORDER BY version_id")
	if err != nil {
		return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []int64

	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
		}

		out = append(out, v)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read applied migrations: %w", err)
	}

	return out, nil
}

func (m *Migrator) ensureChecksumTable(ctx context.Context) error {
	_, err := m.writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+checksumTable+` (
		version     INTEGER PRIMARY KEY,
		name        TEXT    NOT NULL,
		checksum    TEXT    NOT NULL CHECK (length(checksum) = 64),
		recorded_at INTEGER NOT NULL
	) STRICT`)
	if err != nil {
		return fmt.Errorf("sqlite: create %s: %w", checksumTable, err)
	}

	return nil
}

func (m *Migrator) checksum(src *goose.Source) (string, error) {
	b, err := fs.ReadFile(m.fsys, src.Path)
	if err != nil {
		return "", fmt.Errorf("sqlite: read migration %s: %w", src.Path, err)
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}

// verifyChecksums compares the recorded checksum of every applied migration
// with the embedded file. With requireAll, a missing checksum is a mismatch.
func (m *Migrator) verifyChecksums(ctx context.Context, q *sql.DB, requireAll bool) error {
	exists, err := tableExists(ctx, q, checksumTable)
	if err != nil {
		return err
	}

	recorded := map[int64]string{}

	if exists {
		rows, err := q.QueryContext(ctx, "SELECT version, checksum FROM "+checksumTable)
		if err != nil {
			return fmt.Errorf("sqlite: read checksums: %w", err)
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				v   int64
				sum string
			)

			if err := rows.Scan(&v, &sum); err != nil {
				return fmt.Errorf("sqlite: read checksums: %w", err)
			}

			recorded[v] = sum
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("sqlite: read checksums: %w", err)
		}
	}

	versionsExist, err := tableExists(ctx, q, versionTable)
	if err != nil || !versionsExist {
		return err
	}

	applied, err := appliedVersions(ctx, q)
	if err != nil {
		return err
	}

	for _, src := range m.provider.ListSources() {
		if !slices.Contains(applied, src.Version) {
			continue
		}

		want, err := m.checksum(src)
		if err != nil {
			return err
		}

		got, ok := recorded[src.Version]

		switch {
		case !ok && requireAll:
			return fmt.Errorf("%w: no checksum recorded for applied migration %s; run `meshsdr hub migrate` to record it",
				db.ErrChecksumMismatch, path.Base(src.Path))
		case ok && got != want:
			return fmt.Errorf("%w: applied migration %s was modified (recorded sha256 %s, embedded %s)",
				db.ErrChecksumMismatch, path.Base(src.Path), got, want)
		}
	}

	return nil
}

// recordChecksums stores the checksum of every applied migration: replaced
// for the versions in fresh (applied by this run), kept when present for the
// others.
func (m *Migrator) recordChecksums(ctx context.Context, fresh []int64) error {
	if exists, err := tableExists(ctx, m.writer, versionTable); err != nil || !exists {
		return err
	}

	applied, err := appliedVersions(ctx, m.writer)
	if err != nil {
		return err
	}

	now := time.Now().UTC().UnixMilli()

	for _, src := range m.provider.ListSources() {
		if !slices.Contains(applied, src.Version) {
			continue
		}

		sum, err := m.checksum(src)
		if err != nil {
			return err
		}

		onConflict := "DO NOTHING"
		if slices.Contains(fresh, src.Version) {
			onConflict = "DO UPDATE SET name = excluded.name, checksum = excluded.checksum, recorded_at = excluded.recorded_at"
		}

		if _, err := m.writer.ExecContext(ctx,
			"INSERT INTO "+checksumTable+" (version, name, checksum, recorded_at) VALUES (?, ?, ?, ?) ON CONFLICT (version) "+onConflict,
			src.Version, path.Base(src.Path), sum, now); err != nil {
			return fmt.Errorf("sqlite: record checksum of %s: %w", src.Path, err)
		}
	}

	return nil
}
