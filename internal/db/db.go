// Package db is the hub's SQLite database (TECHNICAL_SPEC §7.2 "SQLite
// profile", ADR 0022): one writer connection that serialises every write, a
// pool of read-only connections, WAL journal, the embedded goose migrations
// (internal/db/sqlite/migrations) and the sqlc-generated queries
// (internal/db/sqlite/sqlc). Repositories (internal/<module>/infra/sqlite)
// take a *DB. Domain and application code never import this package: they
// depend on repository interfaces.
package db

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

	"github.com/yohang/mesh-sdr/internal/db/sqlite/migrations"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// busyTimeoutMS is the SQLite busy_timeout (spec: ≥ 5 s).
const busyTimeoutMS = 5000

// Querier runs SQL. It is satisfied by *sql.DB, *sql.Conn and *sql.Tx, and
// matches the DBTX interface of sqlc-generated code.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Options configures Open.
type Options struct {
	// Path is the database file. It is created with mode 0600 when absent.
	Path string
	// MaxReadConnections sizes the read-only pool (db.max_read_connections).
	MaxReadConnections int
	// Migrations overrides the embedded migrations (tests only).
	Migrations fs.FS
	// Logger receives diagnostics.
	Logger *slog.Logger
}

// DB is an open SQLite database.
//
// Writes are serialised (single writer). A unit of work runs in WithinTx;
// the transaction travels in the context, so repositories call
// Reader(ctx)/Writer(ctx) and transparently join it.
type DB struct {
	path     string
	writer   *sql.DB
	reader   *sql.DB
	migrator *Migrator
}

// Open opens the database file, applies and verifies the connection pragmas.
// It does not migrate the schema.
func Open(ctx context.Context, opts Options) (*DB, error) {
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

	d := &DB{path: opts.Path, writer: writer, reader: reader}

	if err := d.verify(ctx); err != nil {
		_ = d.Close()

		return nil, err
	}

	d.migrator, err = newMigrator(writer, reader, opts.Migrations)
	if err != nil {
		_ = d.Close()

		return nil, err
	}

	opts.Logger.DebugContext(ctx, "sqlite database opened",
		slog.String("path", opts.Path), slog.Int("max_read_connections", opts.MaxReadConnections))

	return d, nil
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
func (d *DB) verify(ctx context.Context) error {
	checks := []struct {
		pool   *sql.DB
		pragma string
		want   string
	}{
		{d.writer, "journal_mode", "wal"},
		{d.writer, "foreign_keys", "1"},
		{d.writer, "synchronous", "1"},
		{d.writer, "busy_timeout", fmt.Sprint(busyTimeoutMS)},
		{d.reader, "journal_mode", "wal"},
		{d.reader, "foreign_keys", "1"},
		{d.reader, "query_only", "1"},
	}

	for _, c := range checks {
		var got string
		if err := c.pool.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
			return fmt.Errorf("sqlite: read pragma %s on %s: %w", c.pragma, d.path, err)
		}

		if !strings.EqualFold(got, c.want) {
			return fmt.Errorf("sqlite: pragma %s = %q on %s, want %q (WAL requires a local filesystem)", c.pragma, got, d.path, c.want)
		}
	}

	return nil
}

type txKey struct{}

func txFrom(ctx context.Context) *sql.Tx {
	tx, _ := ctx.Value(txKey{}).(*sql.Tx)

	return tx
}

// Reader returns the querier for reads: the current transaction when ctx
// carries one, the read-only pool otherwise.
func (d *DB) Reader(ctx context.Context) Querier {
	if tx := txFrom(ctx); tx != nil {
		return tx
	}

	return d.reader
}

// Writer returns the querier for writes: the current transaction when ctx
// carries one, the single writer connection otherwise.
func (d *DB) Writer(ctx context.Context) Querier {
	if tx := txFrom(ctx); tx != nil {
		return tx
	}

	return d.writer
}

// WithinTx runs fn in one write transaction, committed when fn returns nil
// and rolled back otherwise. Nested calls join the outer transaction.
func (d *DB) WithinTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if txFrom(ctx) != nil {
		return fn(ctx)
	}

	tx, err := d.writer.BeginTx(ctx, nil)
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

// IsConstraint reports whether err is an SQLite constraint violation
// (SQLITE_CONSTRAINT and its extended codes): the statement failed, the
// transaction goes on.
func IsConstraint(err error) bool {
	var e interface{ Code() int }

	return errors.As(err, &e) && e.Code()&0xff == sqliteConstraint
}

// sqliteConstraint is SQLITE_CONSTRAINT.
const sqliteConstraint = 19

// Migrator returns the schema migrator.
func (d *DB) Migrator() *Migrator { return d.migrator }

// Ping checks that the database answers, with a generated query on the read
// pool.
func (d *DB) Ping(ctx context.Context) error {
	if _, err := sqlc.New(d.reader).Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}

	return nil
}

// Close releases every connection.
func (d *DB) Close() error {
	return errors.Join(d.reader.Close(), d.writer.Close())
}
