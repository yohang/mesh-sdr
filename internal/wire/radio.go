package wire

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/yohang/mesh-sdr/internal/config"
	radioapp "github.com/yohang/mesh-sdr/internal/radio/app"
	radiodomain "github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	"github.com/yohang/mesh-sdr/internal/radio/infra/connector"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// newRadio builds the device module of the node (GRID-002): the device
// manager, the owrx connectors under the process supervisor, the DSP
// engines and the media stream handler. It checks node.runtime_dir (SR-53:
// the node refuses to start on a shared or foreign directory) and sweeps
// the workdirs left by a previous run.
func newRadio(cfg config.Node, logger *slog.Logger, reporter radioapp.Reporter) (*radioapp.Manager, *radiohttp.Streams, error) {
	devices, err := radioDevices(cfg)
	if err != nil {
		return nil, nil, err
	}

	runtimeDir := cfg.Node.RuntimeDir

	// Without a runtime dir (configs built in code), the node runs no tool.
	if runtimeDir == "" && slices.ContainsFunc(devices, (*radiodomain.Device).Usable) {
		return nil, nil, fmt.Errorf("node.runtime_dir is required to run devices")
	}

	var sup *process.Supervisor

	if runtimeDir != "" {
		if sup, err = process.New(process.Options{RuntimeDir: runtimeDir, Logger: component(logger, "radio.infra.process")}); err != nil {
			return nil, nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		n, err := process.SweepSessions(runtimeDir)
		if err != nil {
			return nil, nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		if n > 0 {
			logger.Warn("removed workdirs left by a previous run", slog.Int("count", n))
		}
	}

	lo, hi, err := config.ParsePortRange(cfg.Node.IPCPortRange)
	if err != nil && len(devices) > 0 {
		return nil, nil, fmt.Errorf("node.ipc_port_range: %w", err)
	}

	var ports *connector.Ports
	if err == nil {
		if ports, err = connector.NewPorts(lo, hi); err != nil {
			return nil, nil, fmt.Errorf("node.ipc_port_range: %w", err)
		}
	}

	sources := connector.NewSources(connector.Options{
		Supervisor: sup, Ports: ports, Logger: component(logger, "radio.infra.connector"),
		Tools: connector.Tools{Paths: cfg.Tools.Paths(), Dirs: cfg.Tools.Dirs},
	})

	m, err := radioapp.NewManager(radioapp.Options{
		Devices: devices, Sources: sources, Engines: engine.Factory{Logger: component(logger, "radio.infra.engine")},
		Reporter: reporter, Logger: component(logger, "radio.app.manager"), MaxDemods: cfg.Node.MaxDemods,
	})
	if err != nil {
		return nil, nil, err
	}

	return m, radiohttp.NewStreams(m, component(logger, "radio.http.streams")), nil
}

// radioDevices builds the devices of the node configuration, ordered by id.
func radioDevices(cfg config.Node) ([]*radiodomain.Device, error) {
	out := make([]*radiodomain.Device, 0, len(cfg.Devices))

	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		dev, err := radioDevice(id, cfg.Devices[id])
		if err != nil {
			return nil, fmt.Errorf("devices.%s: %w", id, err)
		}

		out = append(out, dev)
	}

	return out, nil
}

func radioDevice(id string, c config.DeviceConfig) (*radiodomain.Device, error) {
	did, err := shared.NewDeviceID(id)
	if err != nil {
		return nil, err
	}

	typ, err := radiodomain.NewDeviceType(c.Type)
	if err != nil {
		return nil, err
	}

	lo, err := radiodomain.NewFrequency(c.FreqRange.Min.Hz())
	if err != nil {
		return nil, err
	}

	hi, err := radiodomain.NewFrequency(c.FreqRange.Max.Hz())
	if err != nil {
		return nil, err
	}

	rng, err := radiodomain.NewFreqRange(lo, hi)
	if err != nil {
		return nil, err
	}

	rates := make([]radiodomain.SampleRate, 0, len(c.SampleRates))

	for _, r := range c.SampleRates {
		sr, err := radiodomain.NewSampleRate(r)
		if err != nil {
			return nil, err
		}

		rates = append(rates, sr)
	}

	var drv radiodomain.Driver

	if typ.Supported() {
		gain := radiodomain.AutoGain()
		if !c.Driver.RFGain.Auto() {
			if gain, err = radiodomain.NewGain(c.Driver.RFGain.DB()); err != nil {
				return nil, err
			}
		}

		if drv, err = radiodomain.NewDriver(typ, c.Driver.Device, c.Driver.PPM, gain, c.Driver.IQSwap); err != nil {
			return nil, err
		}
	}

	return radiodomain.NewDevice(radiodomain.DeviceParams{
		ID: did, Name: c.Name, Type: typ, Enabled: c.Enabled == nil || *c.Enabled, Range: rng, Rates: rates,
		AlwaysOn: c.AlwaysOn, OperatorCanRetune: c.OperatorCanRetune, AutoRecover: c.AutoRecover == nil || *c.AutoRecover,
		Driver: drv, MaxDemods: c.MaxDemods,
	})
}
