package wire

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/schedules"
)

type fixedSettings map[string]any

func (s fixedSettings) String(key string) string { v, _ := s[key].(string); return v }

func (s fixedSettings) Bool(key string) bool { v, _ := s[key].(bool); return v }

func (s fixedSettings) Int(key string) int { v, _ := s[key].(int); return v }

func (fixedSettings) Duration(string) time.Duration { return 0 }

func (s fixedSettings) Strings(key string) []string { v, _ := s[key].([]string); return v }

type noDevices struct{}

func (noDevices) Device(context.Context, string) (schedules.Device, bool, error) {
	return schedules.Device{}, false, nil
}

func (noDevices) NodeDevices(context.Context, string) ([]schedules.Device, error) { return nil, nil }

func (noDevices) All(context.Context) ([]schedules.Device, error) { return nil, nil }

// TestDesiredStateCarriesSettings: the desired state pushed to the nodes
// carries the listen policy and the waterfall settings (ADR 0026).
func TestDesiredStateCarriesSettings(t *testing.T) {
	a := dbtest.NewSQLite(t)
	logger := slog.New(slog.DiscardHandler)

	values := fixedSettings{
		"listen_policy": "registered", "waterfall.min_db": -110, "waterfall.max_db": -40, "waterfall.palette": "default",
		"fax_lpm": 60, "fax_min_length": 100, "fax_max_length": 900, "fax_color": true,
	}
	d := desiredStates{
		planner:  schedules.NewPlanner(schedules.Deps{Devices: noDevices{}, Logger: logger}),
		presets:  presets.NewService(presets.Deps{Repo: presets.NewPresets(a), Tx: a, Logger: logger}),
		settings: values, listen: gridapp.NewListenPolicies(nil, storeListenPolicy{store: values}, logger),
		now: time.Now,
	}

	st, err := d.Desired(context.Background(), griddomain.MustNodeID("attic"))
	if err != nil {
		t.Fatal(err)
	}

	want := ctl.StateWaterfall{MinDB: -110, MaxDB: -40, Palette: "default"}
	if st.Policy.ListenPolicy != "registered" || st.Policy.Waterfall == nil || *st.Policy.Waterfall != want {
		t.Errorf("policy = %+v", st.Policy)
	}

	fax := ctl.StateFAX{LPM: 60, MinLength: 100, MaxLength: 900, Color: true}
	if d := st.Policy.Decoders; d == nil || d.FAX == nil || *d.FAX != fax {
		t.Errorf("decoders = %+v", st.Policy.Decoders)
	}
}

// The slot decoder settings go from the hub settings through the desired
// state to the node decoders: each WSJT mode gets its effective depth
// (DEC-024).
func TestSlotSettingsHubToNode(t *testing.T) {
	d := stateDecoders(fixedSettings{
		"decoders.max_restarts": 5, "decoders.wsjt_decoding_depth": 2, "decoders.wsjt_decoding_depths.jt65": 1,
		"decoders.fst4_enabled_intervals": []string{"60", "1800"}, "decoders.q65_enabled_combinations": []string{"A30"},
		"decoders.js8_enabled_profiles": []string{"turbo"}, "decoders.js8_decoding_depth": 1, "decoders.dsc_show_errors": true,
	})

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}

	var got ctl.StateDecoders
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	defaults := config.DefaultSettings().Decoders
	s := decoderSettings(&got, defaults)
	if s.WSJTDepths["jt65"] != 1 || s.WSJTDepths["ft8"] != 2 || s.WSJTDepths["q65"] != 2 || len(s.WSJTDepths) != len(wsjtModes) ||
		!slices.Equal(s.FST4Intervals, []int{60, 1800}) || !slices.Equal(s.Q65Combinations, []string{"A30"}) ||
		!slices.Equal(s.JS8Profiles, []string{"turbo"}) || s.JS8Depth != 1 || !s.DSCShowErrors {
		t.Errorf("node settings %+v", s)
	}

	// Without settings, the enabled lists are the hub defaults.
	if s := decoderSettings(nil, defaults); s.WSJTDepths != nil || s.WSJTDepth != 0 || !slices.Equal(s.FST4Intervals, []int{15, 30}) ||
		!slices.Equal(s.FST4WIntervals, []int{120, 300}) || !slices.Equal(s.Q65Combinations, defaults.Q65Combinations) ||
		!slices.Equal(s.JS8Profiles, defaults.JS8Profiles) || !s.DSCShowErrors {
		t.Errorf("no settings: %+v", s)
	}
}
