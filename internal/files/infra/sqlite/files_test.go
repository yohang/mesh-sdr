package sqlite_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/files/domain"
	"github.com/yohang/mesh-sdr/internal/files/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/files/infra/sqlite"
)

func TestFiles(t *testing.T) {
	repotest.Run(t, func(t *testing.T) (domain.Repository, *db.DB) {
		a := dbtest.NewSQLite(t)

		return sqlite.NewFiles(a), a
	})
}
