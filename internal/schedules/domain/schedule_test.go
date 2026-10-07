package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/schedules/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// 2026-10-05 is a Monday.
var monday = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

var (
	presetA = shared.MustParseUUID("0192f2b4-0000-7000-8000-00000000000a")
	presetB = shared.MustParseUUID("0192f2b4-0000-7000-8000-00000000000b")
)

func static(t *testing.T, n int, preset shared.UUID, start, end, days, prio int) *domain.Schedule {
	t.Helper()

	spec, err := domain.NewSpec(domain.Draft{
		DeviceID: "hf", PresetID: preset.String(), StartMinute: &start, EndMinute: &end, DaysOfWeek: &days, Priority: &prio,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := shared.MustParseUUID("0192f2b4-0000-7000-8000-00000000010" + string(rune('0'+n)))

	s, err := domain.NewSchedule(id, spec, monday)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestNewSpec(t *testing.T) {
	if _, err := domain.NewSpec(domain.Draft{DeviceID: "hf", PresetID: presetA.String(), Kind: "daylight", DaylightPhase: "day"}); !errors.Is(err, domain.ErrKindUnsupported) {
		t.Errorf("daylight: %v", err)
	}

	_, err := domain.NewSpec(domain.Draft{DeviceID: "HF!", PresetID: "x", StartMinute: new(10), EndMinute: new(10), DaysOfWeek: new(0)})

	var de *shared.Error
	if !errors.As(err, &de) || !errors.Is(err, domain.ErrInvalidSchedule) {
		t.Fatalf("error = %v", err)
	}

	got := map[string]bool{}
	for _, v := range de.Violations() {
		got[v.Path()] = true
	}

	for _, p := range []string{"device_id", "preset_id", "end_minute", "days_of_week"} {
		if !got[p] {
			t.Errorf("no violation on %s: %v", p, de.Violations())
		}
	}

	if _, err := domain.NewSpec(domain.Draft{DeviceID: "hf", PresetID: presetA.String()}); !errors.Is(err, domain.ErrInvalidSchedule) {
		t.Errorf("static without minutes: %v", err)
	}
}

func TestDisableAndReplace(t *testing.T) {
	s := static(t, 1, presetA, 60, 120, domain.EveryDay, 0)

	if !s.Disable(domain.ReasonDeviceStale, monday) || s.Enabled() || s.Version() != 2 {
		t.Fatalf("disabled = %+v", s.Snapshot())
	}

	if s.Disable(domain.ReasonPresetIncompatible, monday) {
		t.Error("a disabled schedule was disabled again")
	}

	// Removal wins over staleness.
	if !s.Disable(domain.ReasonDeviceRemoved, monday) || s.Version() != 3 {
		t.Errorf("removed = %+v", s.Snapshot())
	}

	// Kept disabled by an edit: still flagged.
	spec, _ := domain.NewSpec(domain.Draft{DeviceID: "hf", PresetID: presetA.String(), StartMinute: new(0), EndMinute: new(30), Enabled: new(false)})
	if err := s.Replace(spec, 3, monday); err != nil {
		t.Fatal(err)
	}

	if r, _ := s.DisabledReason(); r != domain.ReasonDeviceRemoved {
		t.Errorf("reason after a disabled edit = %q", r)
	}

	spec, _ = domain.NewSpec(domain.Draft{DeviceID: "hf", PresetID: presetA.String(), StartMinute: new(0), EndMinute: new(30)})
	if err := s.Replace(spec, 1, monday); !errors.Is(err, domain.ErrVersionConflict) {
		t.Errorf("stale version: %v", err)
	}

	if err := s.Replace(spec, 4, monday); err != nil {
		t.Fatal(err)
	}

	if r, at := s.DisabledReason(); !s.Enabled() || r != domain.ReasonNone || !at.IsZero() {
		t.Errorf("re-enabled = %+v", s.Snapshot())
	}

	back, err := domain.Rehydrate(s.Snapshot())
	if err != nil || back.Window().String() != "0000-0030 UTC" {
		t.Errorf("rehydrated = %v, %v", back, err)
	}
}

type slot struct {
	from, until time.Duration // from monday 00:00
	preset      shared.UUID
}

func check(t *testing.T, tl domain.Timeline, want []slot) {
	t.Helper()

	got := tl.Slots()
	if len(got) != len(want) {
		t.Fatalf("slots = %v, want %d", got, len(want))
	}

	for i, w := range want {
		if !got[i].From.Equal(monday.Add(w.from)) || !got[i].Until.Equal(monday.Add(w.until)) || got[i].Preset != w.preset {
			t.Errorf("slot %d = %v–%v %s, want %v–%v %s", i, got[i].From, got[i].Until, got[i].Preset, monday.Add(w.from),
				monday.Add(w.until), w.preset)
		}
	}
}

func TestEvaluate(t *testing.T) {
	h := time.Hour

	t.Run("overnight wrap belongs to its start day", func(t *testing.T) {
		// 22:00–06:00 on Mondays only (bit 0).
		s := static(t, 1, presetA, 22*60, 6*60, 1, 0)
		check(t, domain.Evaluate([]*domain.Schedule{s}, monday.Add(-12*h), monday.Add(48*h)), []slot{{22 * h, 30 * h, presetA}})
	})

	t.Run("clipped to the horizon", func(t *testing.T) {
		s := static(t, 1, presetA, 0, 2*60, domain.EveryDay, 0)
		tl := domain.Evaluate([]*domain.Schedule{s}, monday.Add(h), monday.Add(25*h))
		check(t, tl, []slot{{h, 2 * h, presetA}, {24 * h, 25 * h, presetA}})

		if !tl.From().Equal(monday.Add(h)) || !tl.Until().Equal(monday.Add(25*h)) {
			t.Errorf("horizon = %v–%v", tl.From(), tl.Until())
		}
	})

	t.Run("priority then earliest start", func(t *testing.T) {
		low := static(t, 1, presetA, 1*60, 5*60, domain.EveryDay, 0)
		high := static(t, 2, presetB, 2*60, 3*60, domain.EveryDay, 1)
		check(t, domain.Evaluate([]*domain.Schedule{low, high}, monday, monday.Add(6*h)), []slot{
			{1 * h, 2 * h, presetA}, {2 * h, 3 * h, presetB}, {3 * h, 5 * h, presetA},
		})

		early := static(t, 3, presetA, 1*60, 4*60, domain.EveryDay, 0)
		late := static(t, 4, presetB, 2*60, 5*60, domain.EveryDay, 0)
		check(t, domain.Evaluate([]*domain.Schedule{late, early}, monday, monday.Add(6*h)), []slot{
			{1 * h, 4 * h, presetA}, {4 * h, 5 * h, presetB},
		})
	})

	t.Run("disabled schedules and empty horizon", func(t *testing.T) {
		s := static(t, 1, presetA, 0, 60, domain.EveryDay, 0)
		s.Disable(domain.ReasonDeviceStale, monday)

		if got := domain.Evaluate([]*domain.Schedule{s}, monday, monday.Add(24*h)).Slots(); len(got) != 0 {
			t.Errorf("slots = %v", got)
		}

		if got := domain.Evaluate(nil, monday, monday).Slots(); len(got) != 0 {
			t.Errorf("empty horizon = %v", got)
		}
	})

	t.Run("week days", func(t *testing.T) {
		// Sundays only (bit 6): the Sunday before monday.
		s := static(t, 1, presetA, 12*60, 13*60, 64, 0)
		check(t, domain.Evaluate([]*domain.Schedule{s}, monday.Add(-24*h), monday.Add(6*24*h)), []slot{{-12 * h, -11 * h, presetA}})
	})

	if got := domain.TimelineStart(monday.Add(90 * time.Minute)); !got.Equal(monday.Add(h)) {
		t.Errorf("timeline start = %v", got)
	}
}
