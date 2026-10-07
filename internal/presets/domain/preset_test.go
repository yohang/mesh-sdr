package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/presets/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func draft() domain.Draft {
	return domain.Draft{Name: "20 m FT8", CenterFreq: 14_074_000, SampRate: 2_048_000, StartMod: "usb"}
}

func violations(t *testing.T, err error) map[string]string {
	t.Helper()

	var de *shared.Error
	if !errors.As(err, &de) || !errors.Is(err, domain.ErrInvalidPreset) {
		t.Fatalf("error = %v, want invalid_preset", err)
	}

	out := map[string]string{}
	for _, v := range de.Violations() {
		out[v.Path()] = string(v.Code())
	}

	return out
}

func TestNewSpecDefaults(t *testing.T) {
	d := draft()
	d.StartMod = ""

	spec, err := domain.NewSpec(d)
	if err != nil {
		t.Fatal(err)
	}

	p, err := domain.NewPreset(shared.MustParseUUID("0192f2b4-0000-7000-8000-000000000001"), spec, 0, t0)
	if err != nil {
		t.Fatal(err)
	}

	if p.StartFreq() != 14_074_000 || p.StartMod().String() != "nfm" || p.TuningStep() != 1000 || p.Slug().String() != "20-m-ft8" ||
		p.Version() != 1 {
		t.Errorf("preset = %+v", p.Snapshot())
	}
}

func TestNewSpecViolations(t *testing.T) {
	bad := domain.Draft{
		Slug: "Bad Slug", Name: " ", Tags: []string{"ok", "\x01"}, CenterFreq: 14_000_000, SampRate: 1_000_000,
		StartFreq: new(int64(15_000_000)), StartMod: "USB!", TuningStep: new(int64(0)), InitialSquelchLevel: new(10),
		InitialNRLevel: new(30), WaterfallLevels: &[2]int{-20, -40},
	}

	got := violations(t, func() error { _, err := domain.NewSpec(bad); return err }())

	for path, code := range map[string]string{
		"slug": "invalid_slug", "name": "required", "tags.1": "invalid_text", "start_freq": "outside_band", "start_mod": "invalid_mode",
		"tuning_step": "out_of_range", "initial_squelch_level": "out_of_range", "initial_nr_level": "out_of_range",
		"waterfall_levels": "out_of_range",
	} {
		if got[path] != code {
			t.Errorf("%s = %q, want %q (all: %v)", path, got[path], code, got)
		}
	}

	if got := violations(t, func() error {
		_, err := domain.NewSpec(domain.Draft{Name: "x", CenterFreq: 0, SampRate: -1})
		return err
	}()); got["center_freq"] != "out_of_range" || got["samp_rate"] != "out_of_range" {
		t.Errorf("violations = %v", got)
	}
}

func TestFits(t *testing.T) {
	spec, err := domain.NewSpec(draft())
	if err != nil {
		t.Fatal(err)
	}

	p, _ := domain.NewPreset(shared.MustParseUUID("0192f2b4-0000-7000-8000-000000000001"), spec, 0, t0)
	hf := domain.DeviceLimits{FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{1_024_000, 2_048_000}}

	if err := p.Fits(hf); err != nil {
		t.Errorf("fits hf: %v", err)
	}

	for name, l := range map[string]domain.DeviceLimits{
		"range": {FreqMin: 14_000_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}},
		"rate":  {FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{1_024_000}},
	} {
		if err := p.Fits(l); !errors.Is(err, domain.ErrPresetIncompatible) {
			t.Errorf("%s: %v, want preset_incompatible", name, err)
		}
	}
}

func TestReplace(t *testing.T) {
	spec, _ := domain.NewSpec(draft())
	p, _ := domain.NewPreset(shared.MustParseUUID("0192f2b4-0000-7000-8000-000000000001"), spec, 3, t0)

	d := draft()
	d.Name = "Renamed"
	next, _ := domain.NewSpec(d)

	if err := p.Replace(next, 2, t0); !errors.Is(err, domain.ErrVersionConflict) {
		t.Errorf("stale version: %v", err)
	}

	if err := p.Replace(next, 1, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// A derived slug never changes the URL of an existing preset.
	if p.Name().String() != "Renamed" || p.Slug().String() != "20-m-ft8" || p.Version() != 2 || !p.UpdatedAt().Equal(t0.Add(time.Minute)) {
		t.Errorf("replaced = %+v", p.Snapshot())
	}

	back, err := domain.Rehydrate(p.Snapshot())
	if err != nil || back.Snapshot().Name != "Renamed" || back.SortOrder() != 3 {
		t.Errorf("rehydrated = %+v, %v", back, err)
	}
}

func TestSlugs(t *testing.T) {
	for in, want := range map[string]string{"  Hello, World! ": "hello-world", "!!!": "preset", "Émetteur 2": "metteur-2"} {
		if got := domain.Slugify(in).String(); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}

	long := domain.Slugify("a123456789b123456789c123456789d123456789e123456789f123456789g123")
	if s := long.WithSuffix(12).String(); len(s) > 64 || s[len(s)-3:] != "-12" {
		t.Errorf("suffixed = %q", s)
	}
}
