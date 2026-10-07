package agent_test

import (
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
		Revision: 1, Presets: presets, Policy: ctl.StatePolicy{ListenPolicy: "anonymous"},
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
		Revision: 2, Presets: map[string]ctl.Preset{"2m": presets["2m"]}, Policy: ctl.StatePolicy{ListenPolicy: "registered"},
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
	if s.Revision() != 0 || s.Policy().ListenPolicy != "registered" {
		t.Errorf("revision %d, policy %+v", s.Revision(), s.Policy())
	}

	// Values the hub never sends are refused: a start frequency outside the
	// band, a zero step, an invalid policy.
	bad := s.Apply(ctl.StateApply{
		Revision: 3, Policy: ctl.StatePolicy{ListenPolicy: "everyone"},
		Presets: map[string]ctl.Preset{"x": {Name: "x", CenterFreq: 14_074_000, SampRate: 2_048_000, StartFreq: 20_000_000, TuningStep: 1}},
		Devices: map[string]ctl.DesiredDevice{"hf": {Presets: []string{"x"}}},
	})
	if len(bad.Errors) != 2 || bad.Errors[0].Code != agent.CodeInvalidState || s.Revision() != 0 {
		t.Errorf("bad = %+v", bad)
	}

	ok := s.Apply(ctl.StateApply{
		Revision: 4, Policy: ctl.StatePolicy{ListenPolicy: "anonymous"},
		Presets: map[string]ctl.Preset{"ft8": presets["ft8"]},
		Devices: map[string]ctl.DesiredDevice{"hf": {Presets: []string{"ft8"}}},
	})
	if len(ok.Errors) != 0 || s.Revision() != 4 {
		t.Errorf("ok = %+v, revision %d", ok, s.Revision())
	}
}
