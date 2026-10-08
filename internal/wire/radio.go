package wire

import (
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	radioapp "github.com/yohang/mesh-sdr/internal/radio/app"
	radiodomain "github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	"github.com/yohang/mesh-sdr/internal/radio/infra/connector"
	"github.com/yohang/mesh-sdr/internal/radio/infra/decoder"
	"github.com/yohang/mesh-sdr/internal/radio/infra/devlog"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// newRadio builds the device module of the node (GRID-002): the device
// manager, the owrx connectors under the process supervisor, the DSP
// engines and the media stream handler. It checks node.runtime_dir (SR-53:
// the node refuses to start on a shared or foreign directory) and sweeps
// the workdirs left by a previous run. The engines and the stream handler
// read the desired state pushed by the hub (WFM de-emphasis, presets,
// waterfall defaults). The sources also probe the device types of the
// capability report (SRC-001). The connectors write their stderr lines to
// the device log (SRC-005). The decoders (DEC-001, DEC-002, DEC-048) run
// their tools under the same supervisor with decoders.process_limits; the
// toolbox probes them for the capability report.
func newRadio(cfg config.Node, logger *slog.Logger, reporter radioapp.Reporter, state radiohttp.DesiredState, deviceLog *devlog.Log,
	dec radioDecoding,
) (*radioapp.Manager, *radiohttp.Streams, *connector.Sources, *decoder.Toolbox, error) {
	devices, err := radioDevices(cfg, logger)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	runtimeDir := cfg.Node.RuntimeDir

	// Without a runtime dir (configs built in code), the node runs no tool.
	if runtimeDir == "" && slices.ContainsFunc(devices, (*radiodomain.Device).Usable) {
		return nil, nil, nil, nil, fmt.Errorf("node.runtime_dir is required to run devices")
	}

	var sup *process.Supervisor

	if runtimeDir != "" {
		if sup, err = process.New(process.Options{RuntimeDir: runtimeDir, Logger: component(logger, "shared.process")}); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		n, err := process.SweepSessions(runtimeDir)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("node.runtime_dir: %w", err)
		}

		if n > 0 {
			logger.Warn("removed workdirs left by a previous run", slog.Int("count", n))
		}
	}

	lo, hi, err := config.ParsePortRange(cfg.Node.IPCPortRange)
	if err != nil && len(devices) > 0 {
		return nil, nil, nil, nil, fmt.Errorf("node.ipc_port_range: %w", err)
	}

	var ports *connector.Ports
	if err == nil {
		if ports, err = connector.NewPorts(lo, hi); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("node.ipc_port_range: %w", err)
		}
	}

	sources := connector.NewSources(connector.Options{
		Supervisor: sup, Ports: ports, Logger: component(logger, "radio.infra.connector"),
		Tools: connector.Tools{Paths: cfg.Tools.Paths(), Dirs: cfg.Tools.Dirs}, DeviceLog: deviceLog.Connector,
	})

	m, err := radioapp.NewManager(radioapp.Options{
		Devices: devices, Sources: sources, Engines: engine.Factory{Logger: component(logger, "radio.infra.engine"), Deemphasis: func() int { return state.Policy().WFMDeemphasis }},
		Reporter: reporter, Logger: component(logger, "radio.app.manager"), MaxDemods: cfg.Node.MaxDemods,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	tools := process.Tools{Paths: cfg.Tools.Paths(), Dirs: cfg.Tools.Dirs}
	toolbox := decoder.NewToolbox(decoder.ToolboxOptions{Supervisor: sup, Tools: tools, Logger: component(logger, "radio.infra.decoder")})
	lim := cfg.Decoders.ProcessLimits
	core := uint64(0)
	runner := decoder.NewRunner(decoder.Options{
		Supervisor: sup, Tools: tools, MaxRestarts: dec.maxRestarts, Reprobe: dec.reprobe, FAX: dec.fax, Logger: component(logger, "radio.infra.decoder"),
		Limits: process.Limits{
			Nice: lim.Nice, OpenFiles: uint64(lim.OpenFiles), AddressSpace: uint64(lim.Memory.Bytes()), Core: &core, NoNewPrivs: true,
		},
	})
	sessions := cfg.Decoders.SessionCap(runtime.NumCPU())
	decoding := radiohttp.Decoding{Decoders: radioapp.NewDecoders(toolbox, runner, sessions, time.Now), Publisher: dec.publisher, Files: dec.files}

	return m, radiohttp.NewStreams(m, state, decoding, component(logger, "radio.http.streams")), sources, toolbox, nil
}

// radioDecoding are the node services the decoders use: the hub
// (decode.batch, the file outbox), the decoding settings of the desired
// state and the capability report.
type radioDecoding struct {
	publisher   radioapp.DecodePublisher
	files       radioapp.FilePublisher
	maxRestarts func() int
	fax         func() decoder.FAXSettings
	reprobe     func()
}

// radioDevices builds the devices of the node configuration, ordered by id.
// A device whose driver or tuning keys are invalid is logged and reported
// failed (invalid_config) instead of stopping the node (SRC-002); the keys
// the hub registry needs (id, name, type, range, rates) are checked with
// the config.
func radioDevices(cfg config.Node, logger *slog.Logger) ([]*radiodomain.Device, error) {
	out := make([]*radiodomain.Device, 0, len(cfg.Devices))

	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		c := cfg.Devices[id]

		dev, err := radioDevice(id, c)
		if err != nil {
			did, idErr := shared.NewDeviceID(id)
			if idErr != nil {
				return nil, fmt.Errorf("devices.%s: %w", id, idErr)
			}

			logger.Error("invalid device configuration: the device is reported failed",
				slog.String("device_id", id), slog.Any("error", err))

			dev = radiodomain.NewInvalidDevice(did, c.Name)
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

	p := radiodomain.DeviceParams{
		ID: did, Name: c.Name, Type: typ, Enabled: c.Enabled == nil || *c.Enabled, Range: rng, Rates: rates,
		AlwaysOn: c.AlwaysOn, OperatorCanRetune: c.OperatorCanRetune, AutoRecover: c.AutoRecover == nil || *c.AutoRecover,
		MaxDemods: c.MaxDemods,
	}

	if hz := c.CenterFreq.Hz(); hz != 0 {
		if p.Center, err = radiodomain.NewFrequency(hz); err != nil {
			return nil, err
		}
	}

	if c.SampleRate != 0 {
		if p.Rate, err = radiodomain.NewSampleRate(c.SampleRate); err != nil {
			return nil, err
		}
	}

	if typ.Supported() {
		if p.Driver, err = radioDriver(typ, c.Driver); err != nil {
			return nil, err
		}
	}

	return radiodomain.NewDevice(p)
}

func radioDriver(typ radiodomain.DeviceType, c config.Driver) (radiodomain.Driver, error) {
	gain := radiodomain.AutoGain()

	if !c.RFGain.Auto() {
		var err error
		if gain, err = radiodomain.NewGain(c.RFGain.DB()); err != nil {
			return radiodomain.Driver{}, err
		}
	}

	ds, err := radiodomain.ParseDirectSampling(c.DirectSampling)
	if err != nil {
		return radiodomain.Driver{}, err
	}

	return radiodomain.NewDriver(typ, radiodomain.DriverSettings{
		Device: c.Device, PPM: c.PPM, Gain: gain, IQSwap: c.IQSwap, BiasTee: c.BiasTee, DirectSampling: ds, LFOOffset: c.LFOOffset,
	})
}
