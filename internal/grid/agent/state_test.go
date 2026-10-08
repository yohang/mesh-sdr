package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

func TestDesiredState(t *testing.T) {
	s := agent.NewDesiredState([]ctl.Device{
		{ID: "hf", FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}},
		{ID: "vhf", FreqMin: 24_000_000, FreqMax: 1_700_000_000, SampleRates: []int64{2_048_000}},
	})

	if s.Revision() != 0 {
		t.Fatalf("revision at boot = %d", s.Revision())
	}

	presets := map[string]ctl.Preset{
		"ft8": {Name: "FT8", CenterFreq: 14_074_000, SampRate: 2_048_000, StartFreq: 14_074_000, TuningStep: 1000},
		"2m":  {Name: "2 m", CenterFreq: 144_800_000, SampRate: 2_048_000, StartFreq: 144_800_000, TuningStep: 1000},
	}

	first := s.Apply(ctl.StateApply{
		Revision: 1, Presets: presets, Policy: ctl.StatePolicy{ListenPolicy: "anonymous", WFMDeemphasis: 50},
		Devices: map[string]ctl.DesiredDevice{
			"hf": {Presets: []string{"ft8"}, ActivePresetID: "ft8", Schedule: ctl.Timeline{
				From: 0, Until: 100, Slots: []ctl.TimelineSlot{{From: 10, Until: 20, PresetID: "ft8"}},
			}},
			"vhf":   {Presets: []string{"2m"}},
			"ghost": {},
		},
	})

	if len(first.Errors) != 1 || first.Errors[0].DeviceID != "ghost" || first.Errors[0].Code != agent.CodeUnknownDevice || s.Revision() != 0 {
		t.Fatalf("first = %+v", first)
	}

	if hf, ok := s.Device("hf"); !ok || hf.ActivePresetID != "ft8" || len(hf.Schedule.Slots) != 1 {
		t.Errorf("hf = %+v", hf)
	}

	// The hub sends a preset the node config refuses for hf, and a broken
	// timeline for vhf: both keep their previous state.
	second := s.Apply(ctl.StateApply{
		Revision: 2, Presets: map[string]ctl.Preset{"2m": presets["2m"]}, Policy: ctl.StatePolicy{ListenPolicy: "registered", WFMDeemphasis: 75},
		Devices: map[string]ctl.DesiredDevice{
			"hf":  {Presets: []string{"2m"}},
			"vhf": {Presets: []string{"2m"}, Schedule: ctl.Timeline{From: 0, Until: 10, Slots: []ctl.TimelineSlot{{From: 5, Until: 20, PresetID: "2m"}}}},
		},
	})

	if len(second.Errors) != 2 || second.Errors[0].Code != agent.CodePresetIncompatible || second.Errors[1].Code != agent.CodeInvalidState {
		t.Fatalf("second = %+v", second)
	}

	if hf, _ := s.Device("hf"); hf.ActivePresetID != "ft8" {
		t.Errorf("hf lost its last good state: %+v", hf)
	}

	if _, ok := s.Preset("ft8"); !ok {
		t.Error("the preset of a kept state was dropped")
	}

	// A refused part leaves the revision unrecorded, so the next welcome
	// asks for the state again.
	if s.Revision() != 0 || s.Policy().ListenPolicy != "registered" || s.Policy().WFMDeemphasis != 75 {
		t.Errorf("revision %d, policy %+v", s.Revision(), s.Policy())
	}

	// Values the hub never sends are refused: a start frequency outside the
	// band, a zero step, an invalid policy.
	bad := s.Apply(ctl.StateApply{
		Revision: 3, Policy: ctl.StatePolicy{ListenPolicy: "everyone", WFMDeemphasis: 50},
		Presets: map[string]ctl.Preset{"x": {Name: "x", CenterFreq: 14_074_000, SampRate: 2_048_000, StartFreq: 20_000_000, TuningStep: 1}},
		Devices: map[string]ctl.DesiredDevice{"hf": {Presets: []string{"x"}}},
	})
	if len(bad.Errors) != 2 || bad.Errors[0].Code != agent.CodeInvalidState || s.Revision() != 0 {
		t.Errorf("bad = %+v", bad)
	}

	ok := s.Apply(ctl.StateApply{
		Revision: 4, Policy: ctl.StatePolicy{ListenPolicy: "anonymous", WFMDeemphasis: 50},
		Presets: map[string]ctl.Preset{"ft8": presets["ft8"]},
		Devices: map[string]ctl.DesiredDevice{"hf": {Presets: []string{"ft8"}}},
	})
	if len(ok.Errors) != 0 || s.Revision() != 4 {
		t.Errorf("ok = %+v, revision %d", ok, s.Revision())
	}

	// The waterfall defaults travel with the policy and are checked like
	// the hub settings; a refused one keeps the previous defaults.
	wf := &ctl.StateWaterfall{MinDB: -100, MaxDB: -30, Palette: "default"}
	if res := s.Apply(ctl.StateApply{Revision: 5, Policy: ctl.StatePolicy{ListenPolicy: "anonymous", Waterfall: wf}}); len(res.Errors) != 0 ||
		*s.Policy().Waterfall != *wf {
		t.Errorf("waterfall = %+v, policy %+v", res, s.Policy())
	}

	for _, bad := range []ctl.StateWaterfall{{MinDB: -20, MaxDB: -88, Palette: "turbo"}, {MinDB: -90, MaxDB: -20, Palette: "rainbow"}} {
		res := s.Apply(ctl.StateApply{Revision: 6, Policy: ctl.StatePolicy{ListenPolicy: "anonymous", Waterfall: &bad}})
		if len(res.Errors) != 1 || s.Revision() != 5 || *s.Policy().Waterfall != *wf {
			t.Errorf("waterfall %+v = %+v, policy %+v", bad, res, s.Policy())
		}
	}
}

func TestDesiredStateListenPolicy(t *testing.T) {
	s := agent.NewDesiredState([]ctl.Device{
		{ID: "open", ListenPolicy: "anonymous"},
		{ID: "closed", ListenPolicy: "registered"},
		{ID: "inherit"},
	})

	// Before the first desired state the global policy is unknown: a device
	// without an override fails closed.
	want := map[string]string{"open": "anonymous", "closed": "registered", "inherit": "registered", "ghost": "registered"}
	for d, p := range want {
		if got := s.ListenPolicy(d); got != p {
			t.Errorf("before any state: %s = %q, want %q", d, got, p)
		}
	}

	for _, global := range []string{"anonymous", "registered"} {
		s.Apply(ctl.StateApply{Revision: 1, Policy: ctl.StatePolicy{ListenPolicy: global}})

		want := map[string]string{"open": "anonymous", "closed": "registered", "inherit": global, "ghost": "registered"}
		for d, p := range want {
			if got := s.ListenPolicy(d); got != p {
				t.Errorf("global %s: %s = %q, want %q", global, d, got, p)
			}
		}
	}
}

// TestDesiredStateOlderHub: a hub that predates wfm_deemphasis sends a
// policy without it; the node accepts the state with the default, and each
// policy field is checked on its own.
func TestDesiredStateOlderHub(t *testing.T) {
	s := agent.NewDesiredState(nil)

	var old ctl.StateApply
	if err := json.Unmarshal([]byte(`{"revision":7,"presets":{},"devices":{},"policy":{"listen_policy":"registered"}}`), &old); err != nil {
		t.Fatal(err)
	}

	if out := s.Apply(old); len(out.Errors) != 0 || s.Revision() != 7 || s.Policy().WFMDeemphasis != agent.DefaultWFMDeemphasis {
		t.Fatalf("older hub: %+v, revision %d, policy %+v", out, s.Revision(), s.Policy())
	}

	s.Apply(ctl.StateApply{Revision: 8, Policy: ctl.StatePolicy{ListenPolicy: "registered", WFMDeemphasis: 75}})

	// An invalid de-emphasis keeps the previous one, the listen policy
	// still applies.
	out := s.Apply(ctl.StateApply{Revision: 9, Policy: ctl.StatePolicy{ListenPolicy: "anonymous", WFMDeemphasis: 60}})
	if len(out.Errors) != 1 || s.Policy() != (ctl.StatePolicy{ListenPolicy: "anonymous", WFMDeemphasis: 75}) || s.Revision() != 8 {
		t.Fatalf("mixed: %+v, policy %+v, revision %d", out, s.Policy(), s.Revision())
	}
}

// The decoding settings (decoders.max_restarts, fax_*) are range-checked.
func TestDesiredStateDecoders(t *testing.T) {
	s := agent.NewDesiredState(nil)

	ok := ctl.StatePolicy{ListenPolicy: "registered", WFMDeemphasis: 50, Decoders: &ctl.StateDecoders{MaxRestarts: 7}}
	if out := s.Apply(ctl.StateApply{Revision: 1, Policy: ok}); len(out.Errors) != 0 || s.Policy().Decoders.MaxRestarts != 7 {
		t.Fatalf("valid: %+v %+v", out, s.Policy())
	}

	bad := ok
	bad.Decoders = &ctl.StateDecoders{MaxRestarts: 0}

	if out := s.Apply(ctl.StateApply{Revision: 2, Policy: bad}); len(out.Errors) != 1 || s.Policy().Decoders.MaxRestarts != 7 {
		t.Fatalf("invalid: %+v %+v", out, s.Policy())
	}

	// The FAX settings (DEC-038).
	fax := ok
	fax.Decoders = &ctl.StateDecoders{MaxRestarts: 5, FAX: &ctl.StateFAX{LPM: 60, MinLength: 100, MaxLength: 800, Color: true}}

	if out := s.Apply(ctl.StateApply{Revision: 3, Policy: fax}); len(out.Errors) != 0 || s.Policy().Decoders.FAX.LPM != 60 {
		t.Fatalf("fax: %+v %+v", out, s.Policy())
	}

	for _, f := range []ctl.StateFAX{{LPM: 20, MinLength: 100, MaxLength: 800}, {LPM: 120, MinLength: 100, MaxLength: 9000}} {
		bad.Decoders = &ctl.StateDecoders{MaxRestarts: 5, FAX: &f}

		if out := s.Apply(ctl.StateApply{Revision: 4, Policy: bad}); len(out.Errors) != 1 || s.Policy().Decoders.FAX.LPM != 60 {
			t.Fatalf("invalid fax %+v: %+v %+v", f, out, s.Policy())
		}
	}

	bad.Decoders = &ctl.StateDecoders{MaxRestarts: 7, DigimodesFFTSize: 3000}

	if out := s.Apply(ctl.StateApply{Revision: 5, Policy: bad}); len(out.Errors) != 1 {
		t.Fatalf("fft size: %+v", out)
	}

	ok.Decoders = &ctl.StateDecoders{MaxRestarts: 7, DigimodesFFTSize: 4096, ShowCW: true}

	if out := s.Apply(ctl.StateApply{Revision: 6, Policy: ok}); len(out.Errors) != 0 || s.Policy().Decoders.DigimodesFFTSize != 4096 || !s.Policy().Decoders.ShowCW {
		t.Fatalf("text settings: %+v %+v", out, s.Policy())
	}
}
