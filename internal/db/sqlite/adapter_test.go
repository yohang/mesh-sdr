package sqlite_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/db/sqlite"
)

func TestAdapterContract(t *testing.T) {
	dbtest.RunAdapterContract(t, dbtest.OpenSQLite)
}

func open(t *testing.T, path string, migrations fstest.MapFS) *sqlite.Adapter {
	t.Helper()

	opts := sqlite.Options{Path: path, MaxReadConnections: 2}
	if migrations != nil {
		opts.Migrations = migrations
	}

	a, err := sqlite.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	return a
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

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", "/tmp/x.db?mode=memory", filepath.Join(t.TempDir(), "missing", "hub.db")} {
		if _, err := sqlite.Open(context.Background(), sqlite.Options{Path: p}); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
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
	m2 = "-- +goose Up\nCREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;\n"
)

func TestCheck(t *testing.T) {
	ctx := context.Background()
	v1 := migrationFS("00001_a.sql", m1)
	v2 := migrationFS("00001_a.sql", m1, "00002_b.sql", m2)

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

	t.Run("modified migration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		if _, err := open(t, path, v1).Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		modified := migrationFS("00001_a.sql", m1+"-- edited\n")

		a := open(t, path, modified)
		if err := a.Migrator().Check(ctx); !errors.Is(err, db.ErrChecksumMismatch) {
			t.Fatalf("Check = %v, want ErrChecksumMismatch", err)
		}

		if _, err := a.Migrator().Up(ctx); !errors.Is(err, db.ErrChecksumMismatch) {
			t.Fatalf("Up = %v, want ErrChecksumMismatch", err)
		}
	})

	t.Run("missing checksum is recorded by up", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		a := open(t, path, v1)

		if _, err := a.Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		if _, err := a.Writer(ctx).ExecContext(ctx, "DELETE FROM schema_migration_checksums"); err != nil {
			t.Fatal(err)
		}

		if err := a.Migrator().Check(ctx); !errors.Is(err, db.ErrChecksumMismatch) {
			t.Fatalf("Check = %v, want ErrChecksumMismatch", err)
		}

		if _, err := a.Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		if err := a.Migrator().Check(ctx); err != nil {
			t.Fatalf("Check after Up = %v", err)
		}
	})

	t.Run("check does not write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		a := open(t, path, v1)

		_ = a.Migrator().Check(ctx)

		var n int
		if err := a.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			t.Fatal(err)
		}

		if n != 0 {
			t.Fatalf("Check created %d schema objects", n)
		}
	})

	t.Run("down", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		a := open(t, path, v2)

		if _, err := a.Migrator().Up(ctx); err != nil {
			t.Fatal(err)
		}

		r, err := a.Migrator().Down(ctx)
		if err != nil || r.Version != 2 || r.Name != "00002_b.sql" {
			t.Fatalf("Down = %+v, %v", r, err)
		}

		if err := a.Migrator().Check(ctx); !errors.Is(err, db.ErrMigrationsPending) {
			t.Fatalf("Check = %v, want pending", err)
		}

		if _, err := a.Migrator().Down(ctx); err != nil {
			t.Fatal(err)
		}

		if _, err := a.Migrator().Down(ctx); !errors.Is(err, db.ErrNoMigration) {
			t.Fatalf("Down on empty = %v, want ErrNoMigration", err)
		}
	})
}

func TestConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	v2 := migrationFS("00001_a.sql", m1, "00002_b.sql", m2)

	adapters := []*sqlite.Adapter{open(t, path, v2), open(t, path, v2), open(t, path, v2)}

	var wg sync.WaitGroup

	applied := make(chan int, len(adapters))
	errs := make(chan error, len(adapters))

	for _, a := range adapters {
		wg.Add(1)

		go func() {
			defer wg.Done()

			r, err := a.Migrator().Up(ctx)
			applied <- len(r)
			errs <- err
		}()
	}

	wg.Wait()
	close(applied)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Up: %v", err)
		}
	}

	total := 0
	for n := range applied {
		total += n
	}

	if total != 2 {
		t.Fatalf("migrations applied %d times, want 2", total)
	}

	if err := adapters[0].Migrator().Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if _, err := os.Stat(path + ".migrate.lock"); err != nil {
		t.Errorf("lock file: %v", err)
	}
}

func TestPragmas(t *testing.T) {
	ctx := context.Background()
	a := open(t, filepath.Join(t.TempDir(), "hub.db"), nil)

	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"busy_timeout": "5000",
		"synchronous":  "1",
		"auto_vacuum":  "2",
	} {
		var got string
		if err := a.Writer(ctx).QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
}
