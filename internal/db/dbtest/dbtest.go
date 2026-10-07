// Package dbtest gives tests a migrated SQLite database in t.TempDir().
package dbtest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
)

// NewSQLite opens a fresh database with every migration applied: the
// starting point of repository tests. It is closed by the test cleanup.
func NewSQLite(t *testing.T) *db.DB {
	t.Helper()

	d, err := db.Open(context.Background(), db.Options{
		Path:               filepath.Join(t.TempDir(), "hub.db"),
		MaxReadConnections: 4,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close sqlite: %v", err)
		}
	})

	if _, err := d.Migrator().Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	return d
}
