package schedules_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/schedules"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestSchedules(t *testing.T) {
	open := func(t *testing.T, presetIDs ...shared.UUID) schedules.Repository {
		a := dbtest.NewSQLite(t)
		repo := presets.NewPresets(a)
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

		for i, id := range presetIDs {
			p := newPreset(t, id, "p"+string(rune(97+i)), i, now)
			if err := repo.Create(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}

		return schedules.NewSchedules(a)
	}

	ctx := context.Background()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	presetA := shared.MustParseUUID("0192f2b4-0000-7000-8000-00000000000a")
	presetB := shared.MustParseUUID("0192f2b4-0000-7000-8000-00000000000b")
	repo := open(t, presetA, presetB)

	newSchedule := func(id, device string, preset shared.UUID, start int) *schedules.Schedule {
		spec, err := schedules.NewSpec(schedules.Draft{
			DeviceID: device, PresetID: preset.String(), StartMinute: &start, EndMinute: new(start + 30), DaysOfWeek: new(31),
			Priority: new(-3),
		})
		if err != nil {
			t.Fatal(err)
		}

		s, err := schedules.NewSchedule(shared.MustParseUUID(id), spec, t0)
		if err != nil {
			t.Fatal(err)
		}

		return s
	}

	a := newSchedule("0192f2b4-0000-7000-8000-000000000101", "hf", presetA, 600)
	b := newSchedule("0192f2b4-0000-7000-8000-000000000102", "hf", presetB, 60)
	c := newSchedule("0192f2b4-0000-7000-8000-000000000103", "vhf", presetA, 0)

	for _, s := range []*schedules.Schedule{a, b, c} {
		if err := repo.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	bad := newSchedule("0192f2b4-0000-7000-8000-000000000104", "hf", shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000ff"), 0)
	if err := repo.Create(ctx, bad); !errors.Is(err, schedules.ErrUnknownPreset) {
		t.Errorf("unknown preset: %v", err)
	}

	got, err := repo.Get(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}

	if gs := got.Snapshot(); gs.Device != "hf" || gs.Preset != presetA || *gs.Start != 600 || *gs.End != 630 || gs.Days != 31 ||
		gs.Priority != -3 || !gs.Enabled || !gs.CreatedAt.Equal(t0) {
		t.Errorf("stored = %+v", gs)
	}

	if list, err := repo.ListByDevice(ctx, shared.MustDeviceID("hf")); err != nil || len(list) != 2 || list[0].ID() != b.ID() {
		t.Errorf("by device = %v, %v", list, err)
	}

	if list, err := repo.ListByPreset(ctx, presetA); err != nil || len(list) != 2 {
		t.Errorf("by preset = %v, %v", list, err)
	}

	if list, err := repo.List(ctx); err != nil || len(list) != 3 {
		t.Errorf("all = %v, %v", list, err)
	}

	got.Disable(schedules.ReasonPresetIncompatible, t0.Add(time.Hour))

	if err := repo.Update(ctx, got, 1); err != nil {
		t.Fatal(err)
	}

	if err := repo.Update(ctx, got, 1); !errors.Is(err, schedules.ErrVersionConflict) {
		t.Errorf("stale update: %v", err)
	}

	again, err := repo.Get(ctx, a.ID())
	if r, at := again.DisabledReason(); err != nil || again.Enabled() || r != schedules.ReasonPresetIncompatible || !at.Equal(t0.Add(time.Hour)) {
		t.Errorf("disabled = %+v, %v", again.Snapshot(), err)
	}

	if err := repo.Delete(ctx, a.ID()); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Get(ctx, a.ID()); !errors.Is(err, schedules.ErrScheduleNotFound) {
		t.Errorf("deleted: %v", err)
	}

	if err := repo.Delete(ctx, a.ID()); !errors.Is(err, schedules.ErrScheduleNotFound) {
		t.Errorf("second delete: %v", err)
	}
}
