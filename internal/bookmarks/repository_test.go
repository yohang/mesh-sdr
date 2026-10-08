package bookmarks

import (
	"context"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestRepository(t *testing.T) {
	ctx := context.Background()
	repo := NewRepository(dbtest.NewSQLite(t))
	ids := shared.NewUUIDv7Generator()

	newID := func() shared.UUID {
		id, err := ids.New(t0)
		if err != nil {
			t.Fatal(err)
		}

		return id
	}

	scope, err := OnDevice(shared.MustDeviceID("hf"))
	if err != nil {
		t.Fatal(err)
	}

	hub, err := NewBookmark(newID(), Draft{Name: "Net", Frequency: 7_100_000, Modulation: "lsb", Scope: scope}, shared.UUID{}, t0)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.Create(ctx, hub); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, hub.ID())
	if err != nil || got.Snapshot().Scope != scope || !got.CreatedBy().IsZero() || !got.UpdatedAt().Equal(t0) || got.Underlying() != "" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	// The unique key holds across origins.
	dup, _ := NewBookmark(newID(), Draft{Name: "Net", Frequency: 7_100_000, Modulation: "lsb"}, shared.UUID{}, t0)
	if err := repo.Create(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate insert = %v", err)
	}

	if taken, err := repo.KeyTaken(ctx, "Net", 7_100_000, "lsb", shared.UUID{}); err != nil || !taken {
		t.Errorf("key taken = %v, %v", taken, err)
	}

	if taken, err := repo.KeyTaken(ctx, "Net", 7_100_000, "lsb", hub.ID()); err != nil || taken {
		t.Errorf("key taken by itself = %v, %v", taken, err)
	}

	// Region rows: one tagged r2, one general.
	for _, e := range []*packEntry{
		{name: "R2 only", frequency: 7_150_000, modulation: "am", packs: []string{"x"}, regions: []string{"r2"}},
		{name: "General", frequency: 7_000_000, modulation: "am", packs: []string{"y"}, general: true, regions: []string{"r2"}},
	} {
		s := e.snapshot()
		s.CreatedAt, s.UpdatedAt = t0, t0

		b, err := Rehydrate(s)
		if err != nil {
			t.Fatal(err)
		}

		if err := repo.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	for region, want := range map[string]int{"r1": 2, "r2": 3} {
		list, err := repo.InRange(ctx, region, 7_000_000, 7_200_000)
		if err != nil || len(list) != want {
			t.Errorf("%s: %v, %v; want %d rows", region, nameList(list), err, want)
		}
	}

	if list, err := repo.InRange(ctx, "r2", 7_120_000, 7_200_000); err != nil || len(list) != 1 || list[0].Regions()[0] != "r2" {
		t.Errorf("range = %v, %v", nameList(list), err)
	}

	// Delete needs the origin: a hub delete never removes a pack row.
	if err := repo.Delete(ctx, hub.ID(), OriginBuiltin); !errors.Is(err, ErrBookmarkNotFound) {
		t.Errorf("delete with another origin = %v", err)
	}

	if err := repo.Delete(ctx, hub.ID(), OriginDB); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Get(ctx, hub.ID()); !errors.Is(err, ErrBookmarkNotFound) {
		t.Errorf("get deleted = %v", err)
	}

	if err := repo.Update(ctx, hub, 1); !errors.Is(err, ErrBookmarkNotFound) {
		t.Errorf("update deleted = %v", err)
	}
}
