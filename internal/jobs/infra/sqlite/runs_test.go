package sqlite_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/jobs/domain"
	"github.com/yohang/mesh-sdr/internal/jobs/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/jobs/infra/sqlite"
)

func TestRuns(t *testing.T) {
	repotest.Run(t, func(t *testing.T) domain.Repository { return sqlite.NewRuns(dbtest.NewSQLite(t)) })
}
