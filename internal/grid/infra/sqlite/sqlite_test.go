package sqlite_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/grid/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
)

func TestRepositories(t *testing.T) {
	repotest.Run(t, func(t *testing.T) repotest.Repos {
		a := dbtest.NewSQLite(t)

		return repotest.Repos{
			Nodes:       sqlite.NewNodeRepository(a),
			Revocations: sqlite.NewRevocationRepository(a),
			Cursors:     sqlite.NewCursorRepository(a),
			Caps:        sqlite.NewCapabilityRepository(a),
		}
	})
}
