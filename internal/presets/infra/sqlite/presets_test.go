package sqlite_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/presets/domain"
	"github.com/yohang/mesh-sdr/internal/presets/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/presets/infra/sqlite"
)

func TestPresets(t *testing.T) {
	repotest.Run(t, func(t *testing.T) domain.Repository { return sqlite.NewPresets(dbtest.NewSQLite(t)) })
}
