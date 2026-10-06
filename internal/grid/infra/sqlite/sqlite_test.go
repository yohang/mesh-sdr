package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestRepositories(t *testing.T) {
	repotest.Run(t, func(t *testing.T) repotest.Repos {
		a := dbtest.NewSQLite(t)

		return repotest.Repos{
			Nodes:       sqlite.NewNodeRepository(a),
			Revocations: sqlite.NewRevocationRepository(a),
			Cursors:     sqlite.NewCursorRepository(a),
			Caps:        sqlite.NewCapabilityRepository(a),
			Devices:     sqlite.NewDeviceRepository(a),
			Conns:       sqlite.NewConnectionRepository(a),
		}
	})
}

func TestEraseUser(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	repo := sqlite.NewConnectionRepository(a)
	ids := shared.NewUUIDv7Generator()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	user, _ := ids.New(now)
	other, _ := ids.New(now)

	open := func(u shared.UUID) shared.UUID {
		id, _ := ids.New(now)

		c, err := domain.NewConnection(domain.ConnectionInfo{
			ID: id, Kind: domain.ConnectionEvents, UserID: u, RoleID: 10, IP: "192.0.2.1", UserAgent: "ua",
		}, now)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := repo.Open(ctx, c); err != nil {
			t.Fatal(err)
		}

		return id
	}

	closedID, openID, othersID := open(user), open(user), open(other)

	closed, _ := repo.Get(ctx, closedID)
	closed.Close(domain.CloseReason("client_close"), now)

	if err := repo.Save(ctx, closed); err != nil {
		t.Fatal(err)
	}

	if err := repo.EraseUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Get(ctx, closedID); err == nil {
		t.Error("closed row of the user kept")
	}

	got, err := repo.Get(ctx, openID)
	if err != nil {
		t.Fatal(err)
	}

	if info := got.Info(); !info.UserID.IsZero() || info.IP != "" || info.UserAgent != "" {
		t.Errorf("open row = %+v", info)
	}

	if o, _ := repo.Get(ctx, othersID); o.Info().UserID != other {
		t.Error("another user's row changed")
	}
}
