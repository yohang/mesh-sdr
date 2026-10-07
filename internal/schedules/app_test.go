package schedules_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/schedules"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// t0 is a Monday (presetA and presetB are declared in schedule_test.go).
var t0 = time.Date(2026, 10, 5, 10, 30, 0, 0, time.UTC)

type devices map[string]schedules.Device

func (d devices) Device(_ context.Context, id string) (schedules.Device, bool, error) {
	v, ok := d[id]

	return v, ok, nil
}

func (d devices) NodeDevices(_ context.Context, node string) ([]schedules.Device, error) {
	var out []schedules.Device

	for _, id := range []string{"hf", "vhf"} {
		if v, ok := d[id]; ok && v.Node == node {
			out = append(out, v)
		}
	}

	return out, nil
}

func (d devices) All(context.Context) ([]schedules.Device, error) {
	var out []schedules.Device
	for _, v := range d {
		out = append(out, v)
	}

	return out, nil
}

// fakePresets fit a device when listed in fits[device].
type fakePresets struct{ fits map[string][]shared.UUID }

func (p fakePresets) Fit(_ context.Context, id shared.UUID, d schedules.Device) (schedules.Fit, error) {
	if id != presetA && id != presetB {
		return schedules.Fit{}, nil
	}

	if slices.Contains(p.fits[d.ID], id) {
		return schedules.Fit{Exists: true}, nil
	}

	return schedules.Fit{Exists: true, Reason: "frequency_range: outside"}, nil
}

func (p fakePresets) Compatible(_ context.Context, d schedules.Device) ([]shared.UUID, error) {
	return p.fits[d.ID], nil
}

type recorder struct{ records []audit.Record }

func (a *recorder) Append(_ context.Context, r audit.Record) error {
	a.records = append(a.records, r)

	return nil
}

type env struct {
	deps    schedules.Deps
	devices devices
	presets *fakePresets
	audit   *recorder
	changed int
}

func newEnv(t *testing.T) *env {
	t.Helper()

	a := dbtest.NewSQLite(t)
	repo := presets.NewPresets(a)

	for i, id := range []shared.UUID{presetA, presetB} {
		if err := repo.Create(context.Background(), newPreset(t, id, "p"+string(rune('a'+i)), i, t0)); err != nil {
			t.Fatal(err)
		}
	}

	e := &env{
		devices: devices{
			"hf":  {ID: "hf", Node: "attic", FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}, SchedulerEnabled: true},
			"vhf": {ID: "vhf", Node: "attic", FreqMin: 24_000_000, FreqMax: 1_700_000_000, SampleRates: []int64{2_048_000}},
		},
		presets: &fakePresets{fits: map[string][]shared.UUID{"hf": {presetA, presetB}, "vhf": {presetB}}},
		audit:   &recorder{},
	}

	e.deps = schedules.Deps{
		Repo: schedules.NewSchedules(a), Tx: a, Devices: e.devices, Presets: e.presets, Audit: e.audit, IDs: shared.NewUUIDv7Generator(),
		Now: func() time.Time { return t0 }, Changed: func(context.Context) { e.changed++ },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	return e
}

func draft(device string, preset shared.UUID, start, end int) schedules.Draft {
	return schedules.Draft{DeviceID: device, PresetID: preset.String(), StartMinute: &start, EndMinute: &end}
}

func TestServiceChecks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	s := schedules.NewService(e.deps)

	e.devices["old"] = schedules.Device{ID: "old", Node: "attic", Stale: true}

	for name, tt := range map[string]struct {
		d    schedules.Draft
		want error
	}{
		"unknown device": {draft("nope", presetA, 0, 60), schedules.ErrUnknownDevice},
		"unknown preset": {draft("hf", shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000ff"), 0, 60), schedules.ErrUnknownPreset},
		"incompatible":   {draft("vhf", presetA, 0, 60), schedules.ErrPresetIncompatible},
		"stale device":   {draft("old", presetA, 0, 60), schedules.ErrDeviceUnavailable},
	} {
		if _, err := s.Create(ctx, tt.d); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
	}

	// A disabled schedule may name a stale device or a preset that does
	// not fit.
	off := draft("vhf", presetA, 0, 60)
	off.Enabled = new(false)

	if _, err := s.Create(ctx, off); err != nil {
		t.Errorf("disabled schedule: %v", err)
	}

	sc, err := s.Create(ctx, draft("hf", presetA, 0, 60))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Replace(ctx, sc.ID().String(), 1, draft("hf", presetB, 60, 120)); err != nil {
		t.Fatal(err)
	}

	if ids, err := s.SchedulesUsing(ctx, presetB); err != nil || len(ids) != 1 || ids[0] != sc.ID() {
		t.Errorf("using B = %v, %v", ids, err)
	}

	// A schedule kept disabled on its own device can be edited after the
	// device left the registry; enabling it, or moving it, cannot.
	gone, err := s.Create(ctx, draft("vhf", presetB, 0, 60))
	if err != nil {
		t.Fatal(err)
	}

	delete(e.devices, "vhf")

	off = draft("vhf", presetB, 0, 90)
	off.Enabled = new(false)

	if _, err := s.Replace(ctx, gone.ID().String(), 1, off); err != nil {
		t.Errorf("disabled edit of a removed device: %v", err)
	}

	if _, err := s.Replace(ctx, gone.ID().String(), 2, draft("vhf", presetB, 0, 90)); !errors.Is(err, schedules.ErrUnknownDevice) {
		t.Errorf("enabling on a removed device: %v", err)
	}

	if _, err := s.Replace(ctx, sc.ID().String(), 2, draft("hf", presetB, 0, 60)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Replace(ctx, sc.ID().String(), 2, draft("hf", presetB, 0, 60)); !errors.Is(err, schedules.ErrVersionConflict) {
		t.Errorf("stale version: %v", err)
	}

	if err := s.Delete(ctx, sc.ID().String()); err != nil {
		t.Fatal(err)
	}

	if len(e.audit.records) != 7 || e.audit.records[0].Actor == audit.System || e.changed != 7 {
		t.Errorf("audit = %+v, changed %d", e.audit.records, e.changed)
	}
}

func TestGuard(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	s := schedules.NewService(e.deps)
	g := schedules.NewGuard(e.deps)

	hfA, _ := s.Create(ctx, draft("hf", presetA, 0, 60))
	hfB, _ := s.Create(ctx, draft("hf", presetB, 60, 120))
	vhf, _ := s.Create(ctx, draft("vhf", presetB, 0, 60))

	reason := func(sc *schedules.Schedule) schedules.DisabledReason {
		t.Helper()

		got, err := e.deps.Repo.Get(ctx, sc.ID())
		if err != nil {
			t.Fatal(err)
		}

		r, _ := got.DisabledReason()

		if r != schedules.ReasonNone && got.Enabled() {
			t.Errorf("schedule %s is enabled with reason %s", sc.ID(), r)
		}

		return r
	}

	// The device's limits changed: preset A no longer fits hf.
	e.presets.fits["hf"] = []shared.UUID{presetB}
	if err := g.DeviceReported(ctx, e.devices["hf"]); err != nil {
		t.Fatal(err)
	}

	if reason(hfA) != schedules.ReasonPresetIncompatible || reason(hfB) != schedules.ReasonNone {
		t.Errorf("after report: %s %s", reason(hfA), reason(hfB))
	}

	if err := g.DevicesStale(ctx, []shared.DeviceID{shared.MustDeviceID("hf")}); err != nil {
		t.Fatal(err)
	}

	if reason(hfB) != schedules.ReasonDeviceStale || reason(hfA) != schedules.ReasonPresetIncompatible {
		t.Errorf("after stale: %s %s", reason(hfA), reason(hfB))
	}

	// A preset change re-checks its schedules.
	e.presets.fits["vhf"] = nil

	disabled, err := g.PresetReplaced(ctx, presetB)
	if err != nil || len(disabled) != 1 || disabled[0] != vhf.ID() || reason(vhf) != schedules.ReasonPresetIncompatible {
		t.Errorf("preset replaced = %v, %v", disabled, err)
	}

	// The safety net disables schedules of devices gone from the registry.
	fresh, err := s.Create(ctx, draft("hf", presetB, 300, 360))
	if err != nil {
		t.Fatal(err)
	}

	delete(e.devices, "hf")

	if n, err := g.Reconcile(ctx); err != nil || n != 1 || reason(fresh) != schedules.ReasonDeviceRemoved {
		t.Errorf("reconcile = %d, %v, %s", n, err, reason(fresh))
	}

	system := 0

	for _, r := range e.audit.records {
		if r.Actor == audit.System && r.Action == schedules.ActionDisable {
			system++
		}
	}

	if system != 4 {
		t.Errorf("%d disable records: %+v", system, e.audit.records)
	}
}

func TestPlanner(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	s := schedules.NewService(e.deps)
	p := schedules.NewPlanner(e.deps)

	if _, err := s.Create(ctx, draft("hf", presetB, 11*60, 12*60)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Create(ctx, draft("vhf", presetB, 11*60, 12*60)); err != nil {
		t.Fatal(err)
	}

	hf := e.devices["hf"]
	hf.ActivePreset = presetB
	e.devices["hf"] = hf

	plans, err := p.Plan(ctx, "attic", t0)
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans = %v, %v", plans, err)
	}

	hfPlan, vhfPlan := plans[0], plans[1]

	if hfPlan.Start != presetB || len(hfPlan.Presets) != 2 || !hfPlan.Timeline.From().Equal(t0.Truncate(time.Hour)) {
		t.Errorf("hf plan = %+v", hfPlan)
	}

	if slots := hfPlan.Timeline.Slots(); len(slots) != 1 || slots[0].Preset != presetB || slots[0].From.Hour() != 11 {
		t.Errorf("hf slots = %v", slots)
	}

	// vhf has scheduler_enabled = false: no timeline; it starts on its
	// first compatible preset.
	if vhfPlan.Start != presetB || len(vhfPlan.Timeline.Slots()) != 0 {
		t.Errorf("vhf plan = %+v", vhfPlan)
	}

	// A stale device gets no timeline either.
	hf.Stale = true
	e.devices["hf"] = hf

	if plans, _ = p.Plan(ctx, "attic", t0); len(plans[0].Timeline.Slots()) != 0 {
		t.Errorf("stale hf slots = %v", plans[0].Timeline.Slots())
	}
}

// newPreset builds a valid preset for tests.
func newPreset(t *testing.T, id shared.UUID, slug string, order int, now time.Time) *presets.Preset {
	t.Helper()

	spec, err := presets.NewSpec(presets.Draft{
		Slug: slug, Name: "Preset " + slug, CenterFreq: 14_074_000, SampRate: 2_048_000, StartMod: "usb",
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
