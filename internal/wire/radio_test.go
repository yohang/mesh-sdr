package wire

import (
	"log/slog"
	"testing"

	"github.com/yohang/mesh-sdr/internal/config"
	radiodomain "github.com/yohang/mesh-sdr/internal/radio/domain"
)

// TestRadioDevicesPassTheConfigAndReportInvalidOnes: the [devices.<id>]
// tuning and driver keys reach the device, and a device with invalid keys
// is reported failed instead of stopping the node (SRC-002).
func TestRadioDevicesPassTheConfigAndReportInvalidOnes(t *testing.T) {
	rng := config.FreqRange{Min: config.MustFrequency("144MHz"), Max: config.MustFrequency("146MHz")}
	cfg := config.Node{Devices: map[string]config.DeviceConfig{
		"good": {
			Name: "Good", Type: "rtl_sdr", FreqRange: rng, SampleRates: []int64{2_400_000, 1_024_000},
			CenterFreq: config.MustFrequency("145.5MHz"), SampleRate: 1_024_000,
			Driver: config.Driver{PPM: 2, BiasTee: true, DirectSampling: "q", LFOOffset: -100_000_000},
		},
		"bad-ppm": {Name: "Bad ppm", Type: "rtl_sdr", FreqRange: rng, SampleRates: []int64{1_024_000}, Driver: config.Driver{PPM: 5000}},
		"bad-tcp": {Name: "Bad tcp", Type: "rtl_tcp", FreqRange: rng, SampleRates: []int64{1_024_000}, Driver: config.Driver{Device: "sdr.lan"}},
		"bad-rate": {
			Name: "Bad rate", Type: "rtl_sdr", FreqRange: rng, SampleRates: []int64{1_024_000}, SampleRate: 2_048_000,
		},
		"bad-ds": {Name: "Bad ds", Type: "rtl_sdr", FreqRange: rng, SampleRates: []int64{1_024_000}, Driver: config.Driver{DirectSampling: "x"}},
	}}

	devices, err := radioDevices(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]*radiodomain.Device{}
	for _, d := range devices {
		byID[d.ID().String()] = d
	}

	good := byID["good"]
	if st, _ := good.State(); st != radiodomain.StateStopped || !good.Usable() {
		t.Fatalf("good device %s", st)
	}

	if tu := good.Tuning(); tu.Center().Hz() != 145_500_000 || tu.Rate().PerSecond() != 1_024_000 {
		t.Fatalf("good tuning %+v", tu)
	}

	if drv := good.Params().Driver; drv.PPM() != 2 || !drv.BiasTee() || drv.DirectSampling() != radiodomain.DirectSamplingQ ||
		drv.HardwareHz(145_500_000) != 45_500_000 {
		t.Fatalf("good driver %+v", drv)
	}

	for _, id := range []string{"bad-ppm", "bad-tcp", "bad-rate", "bad-ds"} {
		d := byID[id]
		if st, reason := d.State(); st != radiodomain.StateFailed || reason != radiodomain.ReasonInvalidConfig || d.Usable() {
			t.Errorf("%s: %s (%s)", id, st, reason)
		}
	}
}
