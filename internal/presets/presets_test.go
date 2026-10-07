package presets_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/presets"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// NewPreset builds a valid preset for tests.
func NewPreset(t *testing.T, id shared.UUID, slug string, order int, now time.Time) *presets.Preset {
	t.Helper()

	spec, err := presets.NewSpec(presets.Draft{
		Slug: slug, Name: "Preset " + slug, Description: "desc", Tags: []string{"b", "a"}, CenterFreq: 14_074_000, SampRate: 2_048_000,
		StartMod: "usb", InitialSquelchLevel: new(-60), WaterfallLevels: &[2]int{-110, -30},
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := presets.NewPreset(id, spec, order, now)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestPresets(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ids := shared.NewUUIDv7Generator()
	repo := presets.NewPresets(dbtest.NewSQLite(t))

	newID := func() shared.UUID {
		id, err := ids.New(t0)
		if err != nil {
			t.Fatal(err)
		}

		return id
	}

	if n, err := repo.NextSortOrder(ctx); err != nil || n != 0 {
		t.Fatalf("empty next order = %d, %v", n, err)
	}

	a, b := NewPreset(t, newID(), "aaa", 1, t0), NewPreset(t, newID(), "bbb", 0, t0)

	for _, p := range []*presets.Preset{a, b} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.Create(ctx, NewPreset(t, newID(), "aaa", 2, t0)); !errors.Is(err, presets.ErrSlugTaken) {
		t.Errorf("duplicate slug: %v", err)
	}

	if taken, err := repo.SlugTaken(ctx, a.Slug(), a.ID()); err != nil || taken {
		t.Errorf("own slug taken = %v, %v", taken, err)
	}

	if taken, err := repo.SlugTaken(ctx, a.Slug(), shared.UUID{}); err != nil || !taken {
		t.Errorf("slug taken = %v, %v", taken, err)
	}

	got, err := repo.Get(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}

	if gs, as := got.Snapshot(), a.Snapshot(); gs.Name != as.Name || *gs.Squelch != -60 || gs.Waterfall[0] != -110 ||
		len(gs.Tags) != 2 || gs.Tags[0] != "a" || !gs.CreatedAt.Equal(t0) {
		t.Errorf("stored = %+v", gs)
	}

	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID() != b.ID() {
		t.Errorf("list = %v, %v", list, err)
	}

	if n, err := repo.NextSortOrder(ctx); err != nil || n != 2 {
		t.Errorf("next order = %d, %v", n, err)
	}

	spec, _ := presets.NewSpec(presets.Draft{Slug: "aaa", Name: "Renamed", CenterFreq: 7_074_000, SampRate: 2_048_000})
	if err := got.Replace(spec, 1, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := repo.Update(ctx, got, 1); err != nil {
		t.Fatal(err)
	}

	if err := repo.Update(ctx, got, 1); !errors.Is(err, presets.ErrVersionConflict) {
		t.Errorf("stale update: %v", err)
	}

	if again, err := repo.Get(ctx, a.ID()); err != nil || again.Name() != "Renamed" || again.Version() != 2 {
		t.Errorf("updated = %v, %v", again, err)
	}

	if err := repo.Delete(ctx, a.ID()); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(ctx, a.ID()); !errors.Is(err, presets.ErrPresetNotFound) {
		t.Errorf("second delete: %v", err)
	}

	if _, err := repo.Get(ctx, a.ID()); !errors.Is(err, presets.ErrPresetNotFound) {
		t.Errorf("get deleted: %v", err)
	}
}
