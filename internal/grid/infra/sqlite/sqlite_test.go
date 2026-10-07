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

// TestDeviceActivePreset: a preset id the hub does not know is stored as
// NULL (devices.active_preset_id references presets, ADR 0020).
func TestDeviceActivePreset(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	node := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://a:1"), now)
	if err := sqlite.NewNodeRepository(a).Create(ctx, node); err != nil {
		t.Fatal(err)
	}

	repo := sqlite.NewDeviceRepository(a)

	d, err := domain.NewReportedDevice(node.ID(), domain.DeviceSpec{
		ID: domain.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000,
		SampleRates: []int64{2_048_000},
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	d.ApplyState(domain.StateRunning, "", nil, shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000ff"), now)

	if err := repo.Save(ctx, d); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, d.ID())
	if err != nil || !got.ActivePreset().IsZero() {
		t.Errorf("active preset = %v, %v", got.ActivePreset(), err)
	}
}
