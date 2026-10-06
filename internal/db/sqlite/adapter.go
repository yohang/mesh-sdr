// Package sqlite is the SQLite dialect adapter (TECHNICAL_SPEC §7.2 "SQLite
// profile"): one writer connection that serialises every write, a pool of
// read-only connections, WAL journal, embedded per-dialect migrations and
// sqlc-generated queries (package sqlite/sqlc).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/migrations"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// busyTimeoutMS is the SQLite busy_timeout (spec: ≥ 5 s).
const busyTimeoutMS = 5000

// Options configures the adapter.
type Options struct {
	// Path is the database file. It is created with mode 0600 when absent.
	Path string
	// MaxReadConnections sizes the read-only pool (db.max_read_connections).
	MaxReadConnections int
	// Migrations overrides the embedded migrations (tests only).
	Migrations fs.FS
	// Logger receives adapter diagnostics.
	Logger *slog.Logger
}

// Adapter implements db.Adapter for SQLite.
type Adapter struct {
	path     string
	writer   *sql.DB
	reader   *sql.DB
	migrator *Migrator
}

var _ db.Adapter = (*Adapter)(nil)

// Open opens the database file, applies and verifies the connection pragmas
// and returns the adapter. It does not migrate the schema.
func Open(ctx context.Context, opts Options) (*Adapter, error) {
	if opts.Path == "" || strings.ContainsRune(opts.Path, 0) {
		return nil, fmt.Errorf("sqlite: invalid database path %q", opts.Path)
	}

	if opts.MaxReadConnections < 1 {
		opts.MaxReadConnections = 1
	}

	if opts.Migrations == nil {
		opts.Migrations = migrations.FS
	}

	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	if err := createFile(opts.Path); err != nil {
		return nil, err
	}

	writer, err := sql.Open("sqlite", dsn(opts.Path, true))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open writer %s: %w", opts.Path, err)
	}

	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)

	reader, err := sql.Open("sqlite", dsn(opts.Path, false))
	if err != nil {
		_ = writer.Close()

		return nil, fmt.Errorf("sqlite: open reader %s: %w", opts.Path, err)
	}

	reader.SetMaxOpenConns(opts.MaxReadConnections)
	reader.SetMaxIdleConns(opts.MaxReadConnections)

	a := &Adapter{path: opts.Path, writer: writer, reader: reader}

	if err := a.verify(ctx); err != nil {
		_ = a.Close()

		return nil, err
	}

	a.migrator, err = newMigrator(opts.Path, writer, reader, opts.Migrations)
	if err != nil {
		_ = a.Close()

		return nil, err
	}

	opts.Logger.DebugContext(ctx, "sqlite database opened",
		slog.String("path", opts.Path), slog.Int("max_read_connections", opts.MaxReadConnections))

	return a, nil
}

// createFile creates the database file with mode 0600 if it does not exist.
func createFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // operator-provided db.dsn
	if errors.Is(err, fs.ErrExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("sqlite: create %s: %w", path, err)
	}

	return f.Close()
}

// dsn builds the modernc.org/sqlite DSN. Pragmas are applied to every new
// connection of the pool.
func dsn(path string, writer bool) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "foreign_keys(ON)")

	if writer {
		// auto_vacuum only takes effect on a new database (before the first table).
		q.Add("_pragma", "auto_vacuum(INCREMENTAL)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		// BEGIN IMMEDIATE: take the write lock at the start of the transaction.
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(ON)")
	}

	return "file:" + uriPath.Replace(path) + "?" + q.Encode()
}

// uriPath escapes the characters that SQLite URI filenames decode or treat
// as delimiters (%, ?, #), so any file path opens the file it names.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// verify checks the pragmas the spec requires on both pools.
func (a *Adapter) verify(ctx context.Context) error {
	checks := []struct {
		pool   *sql.DB
		pragma string
		want   string
	}{
		{a.writer, "journal_mode", "wal"},
		{a.writer, "foreign_keys", "1"},
		{a.writer, "synchronous", "1"},
		{a.writer, "busy_timeout", fmt.Sprint(busyTimeoutMS)},
		{a.reader, "journal_mode", "wal"},
		{a.reader, "foreign_keys", "1"},
		{a.reader, "query_only", "1"},
	}

	for _, c := range checks {
		var got string
		if err := c.pool.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
			return fmt.Errorf("sqlite: read pragma %s on %s: %w", c.pragma, a.path, err)
		}

		if !strings.EqualFold(got, c.want) {
			return fmt.Errorf("sqlite: pragma %s = %q on %s, want %q (WAL requires a local filesystem)", c.pragma, got, a.path, c.want)
		}
	}

	return nil
}

// Dialect implements db.Adapter.
func (a *Adapter) Dialect() db.Dialect { return db.DialectSQLite }

type txKey struct{}

func txFrom(ctx context.Context) *sql.Tx {
	tx, _ := ctx.Value(txKey{}).(*sql.Tx)

	return tx
}

// Reader implements db.Adapter.
func (a *Adapter) Reader(ctx context.Context) db.Querier {
	if tx := txFrom(ctx); tx != nil {
		return tx
	}

	return a.reader
}

// Writer implements db.Adapter.
func (a *Adapter) Writer(ctx context.Context) db.Querier {
	if tx := txFrom(ctx); tx != nil {
		return tx
	}

	return a.writer
}

// WithinTx implements db.Adapter.
func (a *Adapter) WithinTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if txFrom(ctx) != nil {
		return fn(ctx)
	}

	tx, err := a.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()

			panic(p)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("sqlite: rollback: %w", rbErr))
		}

		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}

	return nil
}

// Migrator implements db.Adapter.
func (a *Adapter) Migrator() db.Migrator { return a.migrator }

// Ping implements db.Adapter with a generated query on the read pool.
func (a *Adapter) Ping(ctx context.Context) error {
	if _, err := sqlc.New(a.reader).Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}

	return nil
}

// Close implements db.Adapter.
func (a *Adapter) Close() error {
	return errors.Join(a.reader.Close(), a.writer.Close())
}
