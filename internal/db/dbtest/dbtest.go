// Package dbtest is the contract-test harness of the database adapter layer
// (TECHNICAL_SPEC §7.2 rule 6: one suite runs against every adapter).
//
// RunAdapterContract checks the engine contract (db.Adapter). Repository
// contract suites live next to their interfaces (internal/<module>/infra/
// repotest) and take a factory; each adapter's repository tests call them
// with an adapter from this package, for example NewSQLite.
package dbtest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite"
)

// Factory opens a fresh, empty (unmigrated) database through an adapter.
// The adapter is closed by the factory's test cleanup.
type Factory func(t *testing.T) db.Adapter

// Adapters lists a factory per supported dialect. Add one per new adapter.
func Adapters() map[db.Dialect]Factory {
	return map[db.Dialect]Factory{
		db.DialectSQLite: OpenSQLite,
	}
}

// OpenSQLite opens an empty SQLite database in t.TempDir().
func OpenSQLite(t *testing.T) db.Adapter {
	t.Helper()

	a, err := sqlite.Open(context.Background(), sqlite.Options{
		Path:               filepath.Join(t.TempDir(), "hub.db"),
		MaxReadConnections: 4,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close sqlite: %v", err)
		}
	})

	return a
}

// NewSQLite returns a SQLite adapter on a fresh database with every
// migration applied: the starting point of repository tests.
func NewSQLite(t *testing.T) db.Adapter {
	t.Helper()

	return Migrated(t, OpenSQLite)
}

// Migrated opens a database with factory and applies every migration.
func Migrated(t *testing.T, factory Factory) db.Adapter {
	t.Helper()

	a := factory(t)
	if _, err := a.Migrator().Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	return a
}

// ForEachAdapter runs fn as a subtest for every adapter, on a migrated
// database.
func ForEachAdapter(t *testing.T, fn func(t *testing.T, a db.Adapter)) {
	t.Helper()

	for dialect, factory := range Adapters() {
		t.Run(string(dialect), func(t *testing.T) {
			fn(t, Migrated(t, factory))
		})
	}
}
