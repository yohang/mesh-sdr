// Package radio wires the device module of the node (GRID-002): the device
// manager, the owrx connectors under the process supervisor, the DSP
// engines and the media stream handler.
package radio

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	"github.com/yohang/mesh-sdr/internal/radio/infra/connector"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

// Deps are the dependencies of the module.
type Deps struct {
	Config   config.Node
	Logger   *slog.Logger
	Reporter app.Reporter
}

// Module is the wired device module.
type Module struct {
	Manager *app.Manager
	Streams *radiohttp.Streams
}

func component(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String("component", name))
}

// Wire builds the module. It checks node.runtime_dir (SR-53: the node
// refuses to start on a shared or foreign directory) and sweeps the
// workdirs left by a previous run.
func Wire(d Deps) (*Module, error) {
	devices, err := Devices(d.Config)
	if err != nil {
		return nil, err
	}

	cfg := d.Config
	runtimeDir := cfg.Node.RuntimeDir

	// Without a runtime dir (configs built in code), the node runs no tool.
	if runtimeDir == "" && slices.ContainsFunc(devices, (*domain.Device).Usable) {
		return nil, fmt.Errorf("node.runtime_dir is required to run devices")
	}

	var sup *process.Supervisor

	if runtimeDir != "" {
		if sup, err = process.New(process.Options{RuntimeDir: runtimeDir, Logger: component(d.Logger, "radio.infra.process")}); err != nil {
			return nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		n, err := process.SweepSessions(runtimeDir)
		if err != nil {
			return nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		if n > 0 {
			d.Logger.Warn("removed workdirs left by a previous run", slog.Int("count", n))
		}
	}

	lo, hi, err := config.ParsePortRange(cfg.Node.IPCPortRange)
	if err != nil && len(devices) > 0 {
		return nil, fmt.Errorf("node.ipc_port_range: %w", err)
	}

	var ports *connector.Ports
	if err == nil {
		if ports, err = connector.NewPorts(lo, hi); err != nil {
			return nil, fmt.Errorf("node.ipc_port_range: %w", err)
		}
	}

	sources := connector.NewSources(connector.Options{
		Supervisor: sup, Ports: ports, Logger: component(d.Logger, "radio.infra.connector"),
		Tools: connector.Tools{Paths: cfg.Tools.Paths(), Dirs: cfg.Tools.Dirs},
	})

	m, err := app.NewManager(app.Options{
		Devices: devices, Sources: sources, Engines: engine.Factory{Logger: component(d.Logger, "radio.infra.engine")},
		Reporter: d.Reporter, Logger: component(d.Logger, "radio.app.manager"),
	})
	if err != nil {
		return nil, err
	}

	return &Module{Manager: m, Streams: radiohttp.NewStreams(m, component(d.Logger, "radio.http.streams"))}, nil
}

// Devices builds the devices of the node configuration, ordered by id.
func Devices(cfg config.Node) ([]*domain.Device, error) {
	out := make([]*domain.Device, 0, len(cfg.Devices))

	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		dev, err := device(id, cfg.Devices[id])
		if err != nil {
			return nil, fmt.Errorf("devices.%s: %w", id, err)
		}

		out = append(out, dev)
	}

	return out, nil
}

func device(id string, c config.DeviceConfig) (*domain.Device, error) {
	did, err := domain.NewDeviceID(id)
	if err != nil {
		return nil, err
	}

	typ, err := domain.NewDeviceType(c.Type)
	if err != nil {
		return nil, err
	}

	lo, err := domain.NewFrequency(c.FreqRange.Min.Hz())
	if err != nil {
		return nil, err
	}

	hi, err := domain.NewFrequency(c.FreqRange.Max.Hz())
	if err != nil {
		return nil, err
	}

	rng, err := domain.NewFreqRange(lo, hi)
	if err != nil {
		return nil, err
	}

	rates := make([]domain.SampleRate, 0, len(c.SampleRates))

	for _, r := range c.SampleRates {
		sr, err := domain.NewSampleRate(r)
		if err != nil {
			return nil, err
		}

		rates = append(rates, sr)
	}

	var drv domain.Driver

	if typ.Supported() {
		gain := domain.AutoGain()
		if !c.Driver.RFGain.Auto() {
			if gain, err = domain.NewGain(c.Driver.RFGain.DB()); err != nil {
				return nil, err
			}
		}

		if drv, err = domain.NewDriver(typ, c.Driver.Device, c.Driver.PPM, gain, c.Driver.IQSwap); err != nil {
			return nil, err
		}
	}

	return domain.NewDevice(domain.DeviceParams{
		ID: did, Name: c.Name, Type: typ, Enabled: c.Enabled == nil || *c.Enabled, Range: rng, Rates: rates,
		AlwaysOn: c.AlwaysOn, OperatorCanRetune: c.OperatorCanRetune, AutoRecover: c.AutoRecover == nil || *c.AutoRecover,
		Driver: drv,
	})
}
