package presets_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/presets"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

type recorder struct{ records []audit.Record }

func (a *recorder) Append(_ context.Context, r audit.Record) error {
	a.records = append(a.records, r)

	return nil
}

type schedules struct {
	using    []shared.UUID
	disabled []shared.UUID
	replaced []shared.UUID
}

func (s *schedules) SchedulesUsing(context.Context, shared.UUID) ([]shared.UUID, error) {
	return s.using, nil
}

func (s *schedules) PresetReplaced(_ context.Context, id shared.UUID) ([]shared.UUID, error) {
	s.replaced = append(s.replaced, id)

	return s.disabled, nil
}

func newService(t *testing.T) (*presets.Service, *recorder, *schedules, *int) {
	t.Helper()

	a := dbtest.NewSQLite(t)
	au, sc, changed := &recorder{}, &schedules{}, new(0)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	return presets.NewService(presets.Deps{
		Repo: presets.NewPresets(a), Tx: a, Audit: au, IDs: shared.NewUUIDv7Generator(), Now: func() time.Time { return now },
		Usage: sc, Listener: sc, Changed: func(context.Context) { *changed++ }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), au, sc, changed
}

func TestCreateReplaceDelete(t *testing.T) {
	ctx := context.Background()
	s, au, sc, changed := newService(t)

	d := presets.Draft{Name: "Air band", CenterFreq: 125_000_000, SampRate: 2_400_000}

	a, err := s.Create(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	b, err := s.Create(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	// A derived slug is made unique; an explicit one is refused.
	if a.Slug().String() != "air-band" || b.Slug().String() != "air-band-2" || b.SortOrder() != 1 {
		t.Errorf("slugs = %s %s, order %d", a.Slug(), b.Slug(), b.SortOrder())
	}

	d.Slug = "air-band"
	if _, err := s.Create(ctx, d); !errors.Is(err, presets.ErrSlugTaken) {
		t.Errorf("explicit duplicate slug: %v", err)
	}

	sc.disabled = []shared.UUID{shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000aa")}

	r, err := s.Replace(ctx, a.ID().String(), 1, presets.Draft{Name: "Airband", CenterFreq: 120_000_000, SampRate: 2_400_000})
	if err != nil {
		t.Fatal(err)
	}

	if r.Preset.Version() != 2 || len(r.Disabled) != 1 || len(sc.replaced) != 1 || sc.replaced[0] != a.ID() {
		t.Errorf("replaced = %+v, listener %v", r, sc.replaced)
	}

	if _, err := s.Replace(ctx, a.ID().String(), 1, d); !errors.Is(err, presets.ErrVersionConflict) {
		t.Errorf("stale replace: %v", err)
	}

	sc.using = []shared.UUID{sc.disabled[0]}
	if err := s.Delete(ctx, a.ID().String()); !errors.Is(err, presets.ErrPresetInUse) {
		t.Errorf("delete in use: %v", err)
	}

	sc.using = nil
	if err := s.Delete(ctx, a.ID().String()); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, "not-a-uuid"); !errors.Is(err, presets.ErrPresetNotFound) {
		t.Errorf("invalid id: %v", err)
	}

	actions := []string{}
	for _, r := range au.records {
		actions = append(actions, r.Action)
	}

	if len(actions) != 4 || actions[2] != presets.ActionUpdate || au.records[2].Before["name"] != "Air band" ||
		au.records[2].After["name"] != "Airband" || actions[3] != presets.ActionDelete {
		t.Errorf("audit = %+v", au.records)
	}

	if *changed != 4 {
		t.Errorf("changed %d times", *changed)
	}
}

func TestCompatibleAndCheck(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newService(t)

	hf, _ := s.Create(ctx, presets.Draft{Name: "HF", CenterFreq: 14_074_000, SampRate: 2_048_000})
	if _, err := s.Create(ctx, presets.Draft{Name: "VHF", CenterFreq: 144_800_000, SampRate: 2_048_000}); err != nil {
		t.Fatal(err)
	}

	limits := presets.DeviceLimits{FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}}

	list, err := s.Compatible(ctx, limits)
	if err != nil || len(list) != 1 || list[0].ID() != hf.ID() {
		t.Errorf("compatible = %v, %v", list, err)
	}

	if err := s.Check(ctx, hf.ID(), limits); err != nil {
		t.Errorf("check: %v", err)
	}

	if ok, err := s.Exists(ctx, shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000aa")); ok || err != nil {
		t.Errorf("exists = %v, %v", ok, err)
	}
}
