package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// TestDeviceConfigStored checks that the hub stores the driver values a
// node reports with its devices (SRC-022), read back from the registry.
func TestDeviceConfigStored(t *testing.T) {
	tests := []struct {
		name       string
		config     *ctl.DeviceConfig
		want       *domain.DeviceConfig
		registered bool
	}{
		{
			name:   "manual gain, every value",
			config: &ctl.DeviceConfig{RFGain: "28.5", PPM: -3, BiasTee: true, DirectSampling: "q", IQSwap: true, LFOOffset: -120_000_000},
			want: &domain.DeviceConfig{RFGain: "28.5", PPM: -3, BiasTee: true, DirectSampling: "q", IQSwap: true,
				LFOOffset: -120_000_000},
			registered: true,
		},
		{
			name:       "auto gain",
			config:     &ctl.DeviceConfig{RFGain: "auto", DirectSampling: "off"},
			want:       &domain.DeviceConfig{RFGain: "auto", DirectSampling: "off"},
			registered: true,
		},
		{name: "not reported", registered: true},
		{name: "invalid gain", config: &ctl.DeviceConfig{RFGain: "loud", DirectSampling: "off"}},
		{name: "invalid direct sampling", config: &ctl.DeviceConfig{RFGain: "auto", DirectSampling: "x"}},
		{name: "ppm out of range", config: &ctl.DeviceConfig{RFGain: "auto", PPM: 5000, DirectSampling: "off"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			s := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)
			attic := enrolledNode(t, e)

			d := device("hf", "rtl_sdr")
			d.Config = tt.config

			err := e.db.WithinTx(ctx, func(ctx context.Context) error {
				return s.Sync(ctx, attic, ctl.Capabilities{Devices: []ctl.Device{d}}, e.clock.now())
			})
			if err != nil {
				t.Fatal(err)
			}

			got, err := s.Get(ctx, "hf")
			if !tt.registered {
				if !errors.Is(err, domain.ErrDeviceNotFound) {
					t.Fatalf("device registered: %v", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			c, ok := got.Config()

			switch {
			case tt.want == nil && ok:
				t.Errorf("config = %+v, want none", c)
			case tt.want != nil && (!ok || c != *tt.want):
				t.Errorf("config = %+v %v, want %+v", c, ok, *tt.want)
			}
		})
	}
}

// TestDevicePositionStored checks that the hub stores the position a node
// reports with its devices (MAP-007): the device's own or its node's.
func TestDevicePositionStored(t *testing.T) {
	tests := []struct {
		name       string
		gps        *ctl.Position
		lat        float64
		own        bool
		registered bool
	}{
		{name: "own", gps: &ctl.Position{Lat: 48.85, Lon: 2.35, Source: ctl.PositionDevice}, lat: 48.85, own: true, registered: true},
		{name: "node", gps: &ctl.Position{Lat: 50.63, Lon: 3.06, Source: ctl.PositionNode}, lat: 50.63, registered: true},
		{name: "none", registered: true},
		{name: "invalid", gps: &ctl.Position{Lat: 91, Lon: 3, Source: ctl.PositionNode}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			s := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)
			attic := enrolledNode(t, e)

			d := device("hf", "rtl_sdr")
			d.GPS = tt.gps

			err := e.db.WithinTx(ctx, func(ctx context.Context) error {
				return s.Sync(ctx, attic, ctl.Capabilities{Devices: []ctl.Device{d}}, e.clock.now())
			})
			if err != nil {
				t.Fatal(err)
			}

			got, err := s.Get(ctx, "hf")
			if !tt.registered {
				if !errors.Is(err, domain.ErrDeviceNotFound) {
					t.Fatalf("device registered: %v", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			p, ok := got.Position()

			switch {
			case tt.gps == nil && ok:
				t.Errorf("position = %+v, want none", p)
			case tt.gps != nil && (!ok || p.Lat() != tt.lat || p.Lon() != tt.gps.Lon || p.Own() != tt.own):
				t.Errorf("position = %+v %v", p, ok)
			}
		})
	}
}
