package ctl_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// roundTrip encodes payload in an envelope of typ and decodes it back
// (strict: unknown fields refused). It returns the decoded payload and its
// wire form.
func roundTrip[T any](t *testing.T, typ rxv1.MessageType, payload T) (T, string) {
	t.Helper()

	env, err := rxv1.NewEnvelope(typ, rxv1.CorrelationID{}, 1, payload)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	got, err := rxv1.DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}

	var out T
	if err := got.DecodePayload(&out, true); err != nil {
		t.Fatal(err)
	}

	return out, string(got.Payload())
}

func TestDeviceCodec(t *testing.T) {
	tests := []struct {
		name string
		in   ctl.Device
		want []string
		not  []string
	}{
		{
			name: "with config",
			in: ctl.Device{
				ID: "hf", Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000,
				SampleRates: []int64{2_048_000}, OperatorCanRetune: true,
				Config: &ctl.DeviceConfig{RFGain: "28.5", PPM: -3, BiasTee: true, DirectSampling: "q", IQSwap: true, LFOOffset: -120_000_000},
			},
			want: []string{`"config":{"rf_gain":"28.5","ppm":-3,"bias_tee":true,"direct_sampling":"q","iqswap":true,"lfo_offset":-120000000}`},
		},
		{
			name: "auto gain, defaults",
			in: ctl.Device{
				ID: "vhf", Name: "VHF", Type: "rtl_tcp", FreqMin: 24_000_000, FreqMax: 1_700_000_000, SampleRates: []int64{2_400_000},
				Config: &ctl.DeviceConfig{RFGain: "auto", DirectSampling: "off"},
			},
			want: []string{`"rf_gain":"auto"`, `"direct_sampling":"off"`, `"iqswap":false`},
		},
		{
			name: "no config (older node)",
			in:   ctl.Device{ID: "old", Name: "Old", Type: "rtl_sdr", FreqMin: 1, FreqMax: 2, SampleRates: []int64{1}},
			not:  []string{`"config"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := ctl.Capabilities{Seq: 1, Devices: []ctl.Device{tt.in}}

			got, raw := roundTrip(t, rxv1.TypeNodeCapabilities, caps)
			if !reflect.DeepEqual(got.Devices, caps.Devices) {
				t.Errorf("devices = %+v, want %+v", got.Devices, caps.Devices)
			}

			for _, w := range tt.want {
				if !strings.Contains(raw, w) {
					t.Errorf("payload %s lacks %s", raw, w)
				}
			}

			for _, n := range tt.not {
				if strings.Contains(raw, n) {
					t.Errorf("payload %s has %s", raw, n)
				}
			}
		})
	}
}

func TestDeviceLogCodec(t *testing.T) {
	tests := []struct {
		name string
		in   ctl.DeviceLog
		want string
	}{
		{
			name: "backlog",
			in: ctl.DeviceLog{DeviceID: "hf", Reset: true, Records: []ctl.LogRecord{
				{Time: 1_700_000_000_000, Source: ctl.LogSourceDevice, Class: "starting", Text: "state starting"},
				{Time: 1_700_000_000_500, Source: ctl.LogSourceConnector, Class: "info", Text: "Found 1 device(s) <rtl>"},
			}},
			want: `"reset":true`,
		},
		{
			name: "live",
			in:   ctl.DeviceLog{DeviceID: "hf", Records: []ctl.LogRecord{{Time: 1, Source: ctl.LogSourceConnector, Text: "x"}}},
			want: `"records":[{"t":1,"source":"connector","text":"x"}]`,
		},
		{
			name: "empty backlog",
			in:   ctl.DeviceLog{DeviceID: "hf", Reset: true, Records: []ctl.LogRecord{}},
			want: `"records":[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, raw := roundTrip(t, rxv1.TypeDeviceLog, tt.in)
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("got %+v, want %+v", got, tt.in)
			}

			if !strings.Contains(raw, tt.want) {
				t.Errorf("payload %s lacks %s", raw, tt.want)
			}
		})
	}
}
