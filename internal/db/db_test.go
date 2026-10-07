package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yohang/mesh-sdr/internal/db"
)

func open(t *testing.T, path string, migrations fstest.MapFS) *db.DB {
	t.Helper()

	opts := db.Options{Path: path, MaxReadConnections: 2}
	if migrations != nil {
		opts.Migrations = migrations
	}

	d, err := db.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = d.Close() })

	return d
}

func TestOpenCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	open(t, path, nil)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
}

// URI-special characters in the path must name the file literally.
func TestOpenEscapesPath(t *testing.T) {
	ctx := context.Background()

	for _, name := range []string{"a%20b.db", "a%2fb.db", "q?mode=memory.db", "h#x.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)

			d := open(t, path, nil)
			if _, err := d.Migrator().Up(ctx); err != nil {
				t.Fatal(err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}

			found := false
			for _, e := range entries {
				found = found || e.Name() == name
			}

			if !found {
				t.Fatalf("%q not created; dir has %v", name, entries)
			}

			info, err := os.Stat(path)
			if err != nil || info.Size() == 0 {
				t.Fatalf("database not written to %q: %v", path, err)
			}
		})
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing", "hub.db")} {
		if _, err := db.Open(context.Background(), db.Options{Path: p}); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

func TestPragmas(t *testing.T) {
	ctx := context.Background()
	d := open(t, filepath.Join(t.TempDir(), "hub.db"), nil)

	if err := d.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"busy_timeout": "5000",
		"synchronous":  "1",
		"auto_vacuum":  "2",
	} {
		var got string
		if err := d.Writer(ctx).QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
}

func TestWithinTx(t *testing.T) {
	ctx := context.Background()
	d := open(t, filepath.Join(t.TempDir(), "hub.db"), nil)
	boom := errors.New("boom")

	if _, err := d.Writer(ctx).ExecContext(ctx, "CREATE TABLE probe (id INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	insert := func(ctx context.Context, v string) error {
		_, err := d.Writer(ctx).ExecContext(ctx, "INSERT INTO probe (v) VALUES (?)", v)

		return err
	}

	count := func(ctx context.Context) int {
		var n int
		if err := d.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM probe").Scan(&n); err != nil {
			t.Fatal(err)
		}

		return n
	}

	// Commit, with read-your-writes inside the transaction.
	if err := d.WithinTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, "a"); err != nil {
			return err
		}

		if n := count(ctx); n != 1 {
			t.Errorf("read inside the transaction = %d, want 1", n)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Rollback, including a nested call that joined the outer transaction.
	err := d.WithinTx(ctx, func(ctx context.Context) error {
		if err := d.WithinTx(ctx, func(ctx context.Context) error { return insert(ctx, "inner") }); err != nil {
			return err
		}

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx = %v, want boom", err)
	}

	if n := count(ctx); n != 1 {
		t.Fatalf("count = %d, want 1 (the rolled back write survived)", n)
	}

	if _, err := d.Reader(ctx).ExecContext(ctx, "INSERT INTO probe (v) VALUES ('x')"); err == nil {
		t.Fatal("write through Reader succeeded")
	}
}

func migrationFS(files ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(files); i += 2 {
		fsys[files[i]] = &fstest.MapFile{Data: []byte(files[i+1])}
	}

	return fsys
}

const (
	m1 = "-- +goose Up\nCREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;\n"
	m2 = "-- +goose Up\nCREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;\n-- +goose Down\nDROP TABLE b;\n"
)

func TestMigrator(t *testing.T) {
	ctx := context.Background()
	v1 := migrationFS("00001_a.sql", m1)
	v2 := migrationFS("00001_a.sql", m1, "00002_b.sql", m2)

	t.Run("up, idempotent, status", func(t *testing.T) {
		m := open(t, filepath.Join(t.TempDir(), "hub.db"), nil).Migrator()

		if err := m.Check(ctx); !errors.Is(err, db.ErrMigrationsPending) {
			t.Fatalf("Check before Up = %v, want pending", err)
		}

		if r, err := m.Up(ctx); err != nil || len(r) == 0 {
			t.Fatalf("Up = %v, %v", r, err)
		}

		if err := m.Check(ctx); err != nil {
			t.Fatalf("Check after Up: %v", err)
		}

		if again, err := m.Up(ctx); err != nil || len(again) != 0 {
			t.Fatalf("second Up = %v, %v; want no-op", again, err)
		}

		statuses, err := m.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}

		for _, s := range statuses {
			if !s.Applied || s.Name == "" || s.Version == 0 || s.AppliedAt.IsZero() {
				t.Errorf("status %+v, want applied", s)
			}
		}
	})

	t.Run("pending", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		if _, err := open(t, path, v1).Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		err := open(t, path, v2).Migrator().Check(ctx)

		var verr *db.VersionError
		if !errors.As(err, &verr) || !errors.Is(err, db.ErrMigrationsPending) || verr.Database != 1 || verr.Binary != 2 {
			t.Fatalf("Check = %v, want pending 1 → 2", err)
		}
	})

	t.Run("schema newer than binary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		if _, err := open(t, path, v2).Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		err := open(t, path, v1).Migrator().Check(ctx)

		var verr *db.VersionError
		if !errors.As(err, &verr) || !errors.Is(err, db.ErrSchemaTooNew) || verr.Database != 2 || verr.Binary != 1 {
			t.Fatalf("Check = %v, want too new 2 > 1", err)
		}

		if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
			t.Errorf("message does not name both versions: %v", err)
		}
	})

	t.Run("check does not write", func(t *testing.T) {
		d := open(t, filepath.Join(t.TempDir(), "hub.db"), v1)

		_ = d.Migrator().Check(ctx)

		statuses, err := d.Migrator().Status(ctx)
		if err != nil || len(statuses) != 1 || statuses[0].Applied || statuses[0].Name != "00001_a.sql" {
			t.Fatalf("Status = %+v, %v", statuses, err)
		}

		var n int
		if err := d.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			t.Fatal(err)
		}

		if n != 0 {
			t.Fatalf("Check created %d schema objects", n)
		}
	})

	t.Run("down", func(t *testing.T) {
		m := open(t, filepath.Join(t.TempDir(), "hub.db"), v2).Migrator()

		if _, err := m.Up(ctx); err != nil {
			t.Fatal(err)
		}

		r, err := m.Down(ctx)
		if err != nil || r.Version != 2 || r.Name != "00002_b.sql" {
			t.Fatalf("Down = %+v, %v", r, err)
		}

		if err := m.Check(ctx); !errors.Is(err, db.ErrMigrationsPending) {
			t.Fatalf("Check = %v, want pending", err)
		}

		if _, err := m.Down(ctx); err != nil {
			t.Fatal(err)
		}

		if _, err := m.Down(ctx); !errors.Is(err, db.ErrNoMigration) {
			t.Fatalf("Down on empty = %v, want ErrNoMigration", err)
		}
	})
}
