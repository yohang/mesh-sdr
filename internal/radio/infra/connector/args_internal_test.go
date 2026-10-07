package connector

import (
	"slices"
	"testing"

	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestArgsPassTheDriverSettings(t *testing.T) {
	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)
	gain, _ := domain.NewGain(29.7)

	for name, tc := range map[string]struct {
		s    domain.DriverSettings
		want []string
	}{
		"defaults": {
			s:    domain.DriverSettings{Gain: domain.AutoGain()},
			want: []string{"-d", "0", "-p", "40000", "-c", "40001", "-f", "145000000", "-s", "1024000", "-g", "auto", "-P", "0"},
		},
		"every key": {
			s: domain.DriverSettings{
				Device: "SN42", PPM: -3, Gain: gain, IQSwap: true, BiasTee: true,
				DirectSampling: domain.DirectSamplingQ, LFOOffset: 125_000_000,
			},
			want: []string{
				"-d", "SN42", "-p", "40000", "-c", "40001", "-f", "270000000", "-s", "1024000", "-g", "29.7", "-P", "-3",
				"-i", "-b", "-e", "2",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			drv, err := domain.NewDriver(typ, tc.s)
			if err != nil {
				t.Fatal(err)
			}

			src := &source{p: domain.DeviceParams{ID: shared.MustDeviceID("v"), Type: typ, Driver: drv}, center: 145_000_000}

			if got := src.args(40000, 40001, 1_024_000); !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}
