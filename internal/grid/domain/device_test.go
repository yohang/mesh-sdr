package domain_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestDeviceStatus(t *testing.T) {
	spec := domain.DeviceSpec{
		ID: shared.MustDeviceID("vhf"), Name: "VHF", Type: "rtl_sdr", Enabled: true,
		FreqMin: 144_000_000, FreqMax: 146_000_000, SampleRates: []int64{1_024_000},
	}

	tests := []struct {
		state     domain.RuntimeState
		nodeUp    bool
		listeners int
		want      domain.DeviceStatus
	}{
		{domain.StateStopped, true, 0, domain.DeviceReady},
		{domain.StateRunning, true, 0, domain.DeviceReady},
		{domain.StateRunning, true, 2, domain.DeviceBusy},
		{domain.StateRetuning, true, 1, domain.DeviceBusy},
		{domain.StateRetryWait, true, 1, domain.DeviceFailed},
		{domain.StateFailed, true, 0, domain.DeviceFailed},
		{domain.StateUnavailable, true, 0, domain.DeviceAbsent},
		{domain.StateDisabled, true, 0, domain.DeviceAbsent},
		{domain.StateRunning, false, 1, domain.DeviceAbsent},
	}

	for _, tt := range tests {
		d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), spec, 0, t0)
		if err != nil {
			t.Fatal(err)
		}

		d.ApplyState(tt.state, "", nil, shared.UUID{}, t0)

		if got := d.Status(tt.nodeUp, tt.listeners); got != tt.want {
			t.Errorf("%s up=%v listeners=%d: %s, want %s", tt.state, tt.nodeUp, tt.listeners, got, tt.want)
		}
	}
}
