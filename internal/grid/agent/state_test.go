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
		"ft8": {Name: "FT8", CenterFreq: 14_074_000, SampRate: 2_048_000},
		"2m":  {Name: "2 m", CenterFreq: 144_800_000, SampRate: 2_048_000},
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

	if len(first.Errors) != 1 || first.Errors[0].DeviceID != "ghost" || first.Errors[0].Code != agent.CodeUnknownDevice || s.Revision() != 1 {
		t.Fatalf("first = %+v", first)
	}

	if hf, ok := s.Device("hf"); !ok || hf.ActivePresetID != "ft8" || len(hf.Schedule.Slots) != 1 {
		t.Errorf("hf = %+v", hf)
	}

	// The hub sends a preset the node config refuses for hf, and a broken
	// timeline for vhf: both keep their previous state.
	second := s.Apply(ctl.StateApply{
		Revision: 2, Presets: map[string]ctl.Preset{"2m": presets["2m"]},
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

	if s.Revision() != 2 || s.Policy().ListenPolicy != "" {
		t.Errorf("revision %d, policy %+v", s.Revision(), s.Policy())
	}
}
