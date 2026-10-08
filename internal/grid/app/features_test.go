package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

type deviceList []*domain.Device

func (l deviceList) Get(context.Context, shared.DeviceID) (*domain.Device, error) { return nil, nil }
func (l deviceList) List(context.Context) ([]*domain.Device, error)               { return l, nil }
func (l deviceList) ListByNode(context.Context, domain.NodeID) ([]*domain.Device, error) {
	return nil, nil
}
func (l deviceList) Save(context.Context, *domain.Device) error          { return nil }
func (l deviceList) SetNodeOffline(context.Context, domain.NodeID) error { return nil }

type reports map[domain.NodeID]domain.CapabilityReport

func (r reports) Get(_ context.Context, id domain.NodeID) (domain.CapabilityReport, error) {
	rep, ok := r[id]
	if !ok {
		return domain.CapabilityReport{}, domain.ErrCapabilitiesNotReported
	}

	return rep, nil
}

// links is a fixed set of connected nodes.
type links []domain.NodeID

func (l links) Connected() []domain.NodeID { return l }

type fixedPolicy struct {
	v   string
	err error
}

func (p fixedPolicy) ListenPolicy(context.Context) (string, error) { return p.v, p.err }

func TestFeatures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	dev := func(node, id, typ string, enabled bool, policy string) *domain.Device {
		t.Helper()

		d, err := domain.NewReportedDevice(domain.MustNodeID(node), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id + " name", Type: typ, Enabled: enabled, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: policy,
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	report := func(node string, caps ...string) domain.CapabilityReport {
		t.Helper()

		var rows []domain.Capability

		for _, c := range caps {
			key, available := c, true
			if c[0] == '!' {
				key, available = c[1:], false
			}

			status := domain.CapabilityOK
			if !available {
				status = domain.CapabilityMissing
			}

			row, err := domain.NewCapability(key, available, status, "", json.RawMessage(`{}`), "")
			if err != nil {
				t.Fatal(err)
			}

			rows = append(rows, row)
		}

		r, err := domain.NewCapabilityReport(domain.MustNodeID(node), "h", "1.0.0", nil, json.RawMessage(`{}`), json.RawMessage(`{}`), now, rows)
		if err != nil {
			t.Fatal(err)
		}

		return r
	}

	devices := deviceList{
		dev("attic", "hf", "rtl_sdr", true, ""),
		dev("attic", "off", "rtl_sdr", false, ""),
		dev("attic", "vhf", "soapy:airspy", true, "registered"),
		dev("garden", "uhf", "rtl_sdr", true, "anonymous"),
	}
	caps := reports{
		domain.MustNodeID("attic"): report("attic", "mode:wspr", "mode:ft8", "!mode:dmr", "driver:rtl_sdr", "!driver:soapy:airspy", "tool:jt9"),
	}

	summary := func(p fixedPolicy) []string {
		t.Helper()

		got, err := app.NewFeatures(app.FeaturesDeps{
			Devices: devices, Caps: caps, Policy: p, Links: links{domain.MustNodeID("garden")},
		}).Summary(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		var out []string
		for _, d := range got.Devices {
			out = append(out, fmt.Sprintf("%s@%s %s %v node_online=%v", d.ID, d.Node, d.ListenPolicy, d.Modes, d.NodeOnline))
		}

		return out
	}

	want := []string{
		"hf@attic anonymous [ft8 wspr] node_online=false",
		"vhf@attic registered [] node_online=false", // driver missing
		"uhf@garden anonymous [] node_online=true",  // node connected, not reported yet
	}
	if got := summary(fixedPolicy{v: "anonymous"}); !slices.Equal(got, want) {
		t.Errorf("summary = %v, want %v", got, want)
	}

	// The global policy applies where no device override exists; an
	// unreadable policy fails closed.
	for _, p := range []fixedPolicy{{v: "registered"}, {err: errors.New("down")}, {v: "bogus"}} {
		if got := summary(p); got[0] != "hf@attic registered [ft8 wspr] node_online=false" || got[2] != "uhf@garden anonymous [] node_online=true" {
			t.Errorf("policy %+v: summary = %v", p, got)
		}
	}
}

type listenerCounts map[string]int

func (l listenerCounts) ListenersByDevice(context.Context) (map[string]int, error) { return l, nil }

type nodeList []*domain.Node

func (l nodeList) List(context.Context) ([]*domain.Node, error) { return l, nil }

type latest map[domain.NodeID]app.LoadSample

func (l latest) Latest(id domain.NodeID) (app.LoadSample, bool) {
	s, ok := l[id]

	return s, ok
}

// TestFeaturesPickerFields covers what the device picker shows (UI-021):
// the node names, the device state, its listeners and active preset, and
// the public telemetry of online nodes only (RX-035).
func TestFeaturesPickerFields(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	preset := shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd")

	dev := func(node, id string) *domain.Device {
		t.Helper()

		d, err := domain.NewReportedDevice(domain.MustNodeID(node), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{1},
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	hf, vhf := dev("attic", "hf"), dev("garden", "vhf")
	hf.ApplyState(domain.StateRunning, "", nil, preset, now)

	temp, battery := 41.5, 87.0
	got, err := app.NewFeatures(app.FeaturesDeps{
		Devices: deviceList{hf, vhf}, Caps: reports{}, Policy: fixedPolicy{v: "anonymous"},
		Links:     links{domain.MustNodeID("attic")},
		Nodes:     nodeList{domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://attic:8074"), now)},
		Listeners: listenerCounts{"hf": 3},
		Telemetry: latest{
			domain.MustNodeID("attic"):  {CPU: 0.25, TempC: &temp, Battery: &battery},
			domain.MustNodeID("garden"): {CPU: 0.5},
		},
		PresetName: func(_ context.Context, id shared.UUID) string {
			if id == preset {
				return "Airband"
			}

			return ""
		},
	}).Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Devices) != 2 || len(got.Nodes) != 2 {
		t.Fatalf("summary = %+v", got)
	}

	d := got.Devices[0]
	if d.State != domain.StateRunning || d.Listeners != 3 || d.ActivePreset != preset || d.PresetName != "Airband" {
		t.Errorf("hf = %+v", d)
	}

	if d := got.Devices[1]; d.Listeners != 0 || !d.ActivePreset.IsZero() || d.PresetName != "" {
		t.Errorf("vhf = %+v", d)
	}

	attic, garden := got.Nodes[0], got.Nodes[1]
	if attic.Name != "Attic" || !attic.Online || attic.Telemetry == nil || attic.Telemetry.CPU != 0.25 ||
		*attic.Telemetry.TempC != temp || *attic.Telemetry.Battery != battery {
		t.Errorf("attic = %+v", attic)
	}

	// An unnamed node goes by its id; an offline one has no telemetry.
	if garden.Name != "garden" || garden.Online || garden.Telemetry != nil {
		t.Errorf("garden = %+v", garden)
	}
}

func TestListenPolicies(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	dev := func(id string, enabled bool, policy string) *domain.Device {
		t.Helper()

		d, err := domain.NewReportedDevice(domain.MustNodeID("n1"), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: enabled, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: policy,
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	devices := deviceList{dev("open", true, "anonymous"), dev("closed", true, ""), dev("off", false, "anonymous")}

	for _, tt := range []struct {
		name          string
		global        fixedPolicy
		any           bool
		open, closedD string
	}{
		{"registered globally", fixedPolicy{v: "registered"}, true, "anonymous", "registered"},
		{"anonymous globally", fixedPolicy{v: "anonymous"}, true, "anonymous", "anonymous"},
		{"unreadable global", fixedPolicy{err: errors.New("down")}, true, "anonymous", "registered"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := app.NewListenPolicies(devices, tt.global)

			if got, err := p.AnyAnonymous(ctx); err != nil || got != tt.any {
				t.Errorf("AnyAnonymous = %v, %v", got, err)
			}

			if got, ok, err := p.Device(ctx, "open"); err != nil || !ok || got != tt.open {
				t.Errorf("open = %q %v %v", got, ok, err)
			}

			if got, ok, err := p.Device(ctx, "closed"); err != nil || !ok || got != tt.closedD {
				t.Errorf("closed = %q %v %v", got, ok, err)
			}

			if _, ok, _ := p.Device(ctx, "off"); ok {
				t.Error("a disabled device is listenable")
			}

			if _, ok, _ := p.Device(ctx, "nope"); ok {
				t.Error("an unknown device is listenable")
			}
		})
	}

	all, err := app.NewListenPolicies(devices, fixedPolicy{v: "registered"}).Effective(ctx)
	if err != nil || len(all) != 2 || all["open"] != "anonymous" || all["closed"] != "registered" {
		t.Errorf("Effective = %v, %v", all, err)
	}

	none := app.NewListenPolicies(deviceList{dev("closed", true, "")}, fixedPolicy{v: "registered"})
	if got, _ := none.AnyAnonymous(ctx); got {
		t.Error("AnyAnonymous without an anonymous device")
	}
}
