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
)

type deviceList []*domain.Device

func (l deviceList) Get(context.Context, domain.DeviceID) (*domain.Device, error) { return nil, nil }
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
			ID: domain.MustDeviceID(id), Name: id + " name", Type: typ, Enabled: enabled, FreqMin: 1, FreqMax: 2,
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

		got, err := app.NewFeatures(devices, caps, p).Summary(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		var out []string
		for _, d := range got {
			out = append(out, fmt.Sprintf("%s@%s %s %v", d.ID, d.Node, d.ListenPolicy, d.Modes))
		}

		return out
	}

	want := []string{
		"hf@attic anonymous [ft8 wspr]",
		"vhf@attic registered []", // driver missing
		"uhf@garden anonymous []", // node not reported yet
	}
	if got := summary(fixedPolicy{v: "anonymous"}); !slices.Equal(got, want) {
		t.Errorf("summary = %v, want %v", got, want)
	}

	// The global policy applies where no device override exists; an
	// unreadable policy fails closed.
	for _, p := range []fixedPolicy{{v: "registered"}, {err: errors.New("down")}, {v: "bogus"}} {
		if got := summary(p); got[0] != "hf@attic registered [ft8 wspr]" || got[2] != "uhf@garden anonymous []" {
			t.Errorf("policy %+v: summary = %v", p, got)
		}
	}
}

func TestListenPolicies(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	dev := func(id string, enabled bool, policy string) *domain.Device {
		t.Helper()

		d, err := domain.NewReportedDevice(domain.MustNodeID("n1"), domain.DeviceSpec{
			ID: domain.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: enabled, FreqMin: 1, FreqMax: 2,
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
