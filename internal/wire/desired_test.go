package wire

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/schedules"
)

type fixedSettings map[string]any

func (s fixedSettings) String(key string) string { v, _ := s[key].(string); return v }

func (s fixedSettings) Int(key string) int { v, _ := s[key].(int); return v }

func (fixedSettings) Duration(string) time.Duration { return 0 }

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

	d := desiredStates{
		planner: schedules.NewPlanner(schedules.Deps{Devices: noDevices{}, Logger: logger}),
		presets: presets.NewService(presets.Deps{Repo: presets.NewPresets(a), Tx: a, Logger: logger}),
		settings: fixedSettings{
			"listen_policy": "registered", "waterfall.min_db": -110, "waterfall.max_db": -40, "waterfall.palette": "default",
		},
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
}
