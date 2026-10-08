package http

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// bandDevices serves Get from a map; the other methods are not used.
type bandDevices struct {
	Devices

	byID map[string]*domain.Device
}

func (f bandDevices) Get(_ context.Context, id string) (*domain.Device, error) {
	if d, ok := f.byID[id]; ok {
		return d, nil
	}

	return nil, domain.ErrDeviceNotFound
}

// TestBandOf: without a preset band, Admin › Connections names the band
// plan band of the tuned frequency, else of the device's centre; "—"
// outside every band or when nothing is known.
func TestBandOf(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ptr := func(v int64) *int64 { return &v }

	device := func(id string, centre *int64) *domain.Device {
		d, err := domain.RehydrateDevice(domain.DeviceSnapshot{
			ID: id, Node: "attic", Name: id, Type: "rtl_sdr", FreqMin: 1, FreqMax: 2_000_000_000,
			SampleRates: []int64{1_024_000}, State: domain.StateRunning, CenterFreq: centre,
		})
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	devices := bandDevices{byID: map[string]*domain.Device{
		"vhf":    device("vhf", ptr(144_512_000)),
		"hf":     device("hf", ptr(9_000_000)),
		"silent": device("silent", nil),
	}}

	// A two-band plan: 2m and 70cm.
	bandAt := func(_ context.Context, hz int64) string {
		switch {
		case hz >= 144_000_000 && hz <= 146_000_000:
			return "2m"
		case hz >= 430_000_000 && hz <= 440_000_000:
			return "70cm"
		}

		return ""
	}

	conn := func(deviceID string, tuned *int64) *domain.Connection {
		c, err := domain.RehydrateConnection(domain.ConnectionSnapshot{
			Info: domain.ConnectionInfo{
				ID: shared.MustParseUUID("01a11bb0-5c16-77ed-97ea-d72b8c93a6a2"), Kind: domain.ConnectionMedia,
				NodeID: "attic", DeviceID: deviceID,
			},
			TunedFreq: tuned, OpenedAt: t0, LastHeartbeat: t0,
		})
		if err != nil {
			t.Fatal(err)
		}

		return c
	}

	tests := []struct {
		name   string
		bandAt func(context.Context, int64) string
		c      *domain.Connection
		want   string
	}{
		{"tuned frequency", bandAt, conn("vhf", ptr(435_000_000)), "70cm"},
		{"device centre", bandAt, conn("vhf", nil), "2m"},
		{"outside every band", bandAt, conn("hf", nil), "—"},
		{"no centre reported", bandAt, conn("silent", nil), "—"},
		{"unknown device", bandAt, conn("gone", nil), "—"},
		{"no device", bandAt, conn("", nil), "—"},
		{"no band plan", nil, conn("vhf", nil), "—"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewAdminModule(AdminDeps{Devices: devices, BandAt: tt.bandAt})
			if got := m.bandOf(context.Background(), tt.c, map[string]*domain.Device{}); got != tt.want {
				t.Errorf("bandOf = %q, want %q", got, tt.want)
			}
		})
	}
}
