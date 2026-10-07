package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	presetsrepotest "github.com/yohang/mesh-sdr/internal/presets/infra/repotest"
	presetssqlite "github.com/yohang/mesh-sdr/internal/presets/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/schedules/domain"
	"github.com/yohang/mesh-sdr/internal/schedules/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/schedules/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestSchedules(t *testing.T) {
	repotest.Run(t, func(t *testing.T, presets ...shared.UUID) domain.Repository {
		a := dbtest.NewSQLite(t)
		repo := presetssqlite.NewPresets(a)
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

		for i, id := range presets {
			p := presetsrepotest.NewPreset(t, id, "p"+string(rune('a'+i)), i, now)
			if err := repo.Create(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}

		return sqlite.NewSchedules(a)
	})
}
