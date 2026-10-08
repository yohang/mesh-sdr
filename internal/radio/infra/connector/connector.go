// Package connector runs the owrx_connector processes of the node devices
// (TECHNICAL_SPEC §8.2 "Driver strategy"): rtl_connector for rtl_sdr and
// rtl_tcp_connector for rtl_tcp. The node spawns the connector through the
// supervisor with argv only, reads CF32 IQ from its loopback IQ socket and
// retunes it live through its loopback control socket (key:value lines).
package connector

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

// Timeouts of §8.2 "Lifecycle".
const (
	StartTimeout = 15 * time.Second
	StallTimeout = 3 * time.Second
	StopGrace    = 5 * time.Second
	ProbeTimeout = 5 * time.Second
	// MaxBlock is the largest IQ block delivered to the sink.
	MaxBlock = 16384
	// dialInterval paces the connection attempts to a starting connector.
	dialInterval = 50 * time.Millisecond
)

// Tools resolves the external programs (ADR 0017 decision 10).
type Tools struct {
	// Paths are the tools.<name> absolute paths.
	Paths map[string]string
	// Dirs is tools.dirs: the search path, and the child's PATH.
	Dirs []string
}

// Resolve returns the absolute path of a tool: tools.<name> when set,
// otherwise the first executable of that name in Dirs.
func (t Tools) Resolve(name string) (string, error) {
	if p := t.Paths[name]; p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("tools.%s: %q is not absolute", name, p)
		}

		return filepath.Clean(p), nil
	}

	p, err := process.ResolveTool(name, t.Dirs)
	if err != nil {
		return "", fmt.Errorf("tool %s not found in tools.dirs: %w", name, err)
	}

	return p, nil
}

// connectorRules classify connector stderr lines (§8.2 rule 5).
var connectorRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)(no (supported )?devices? found|device lost|failed to open|LIBUSB_ERROR_NO_DEVICE|disconnected)`), Class: process.ClassDeviceLost},
	{Pattern: regexp.MustCompile(`(?i)(libusb|usb_claim_interface|usb error)`), Class: process.ClassUSBError},
	{Pattern: regexp.MustCompile(`(?i)(overflow|lost samples|buffer full|dropping)`), Class: process.ClassOverflow},
	{Pattern: regexp.MustCompile(`(?i)(warning|failed)`), Class: process.ClassWarn},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// maxInstanceID is the longest supervisor instance id (its workdir name).
const maxInstanceID = 64

// instanceID returns the supervisor instance id of a device: prefix-id, or,
// when a long device id (up to 63 characters) would not fit, its head and
// the first 8 hex digits of its SHA-256, which keeps ids distinct.
func instanceID(prefix, id string) string {
	s := prefix + "-" + id
	if len(s) <= maxInstanceID {
		return s
	}

	sum := sha256.Sum256([]byte(id))
	head := maxInstanceID - len(prefix) - 1 - 9

	return prefix + "-" + id[:head] + "-" + hex.EncodeToString(sum[:4])
}

// Options configure the sources.
type Options struct {
	Supervisor *process.Supervisor
	Tools      Tools
	Ports      *Ports
	Logger     *slog.Logger
	// Policy is the restart policy (default process.DevicePolicy, §8.2).
	Policy *process.RestartPolicy
	// Timeouts override the §8.2 values (tests).
	StartTimeout, StallTimeout time.Duration
}

// Sources implements app.Sources with owrx_connector processes.
type Sources struct {
	o Options

	mu sync.Mutex
	// unavailable is the reason last logged for each driver type.
	unavailable map[string]string
}

// NewSources returns the sources.
func NewSources(o Options) *Sources {
	if o.StartTimeout <= 0 {
		o.StartTimeout = StartTimeout
	}

	if o.StallTimeout <= 0 {
		o.StallTimeout = StallTimeout
	}

	return &Sources{o: o, unavailable: map[string]string{}}
}

func (s *Sources) tool(t domain.DeviceType) (string, error) {
	if !t.Supported() {
		return "", fmt.Errorf("device type %s: %w", t, app.ErrSourceUnavailable)
	}

	return s.o.Tools.Resolve(t.Tool())
}

// Probe implements app.Sources: the connector must start (--version) within
// ProbeTimeout (§8.4 capability probing: a batch instance).
func (s *Sources) Probe(ctx context.Context, p domain.DeviceParams) error {
	return s.probe(ctx, instanceID("probe", p.ID.String()), p.Type)
}

// Driver is the availability of a registered device type on this node
// (SRC-001); Reason says why an unavailable one cannot run.
type Driver struct {
	Type      string
	Available bool
	Reason    string
}

// Drivers probes the connector of every registered device type, like
// Probe does for a device. An unavailable type is logged when its reason
// changes, not at every report.
func (s *Sources) Drivers(ctx context.Context) []Driver {
	types := domain.SupportedTypes()
	out := make([]Driver, 0, len(types))

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range types {
		d := Driver{Type: t.String(), Available: true}

		if err := s.probe(ctx, "driver-"+t.String(), t); err != nil {
			d.Available, d.Reason = false, err.Error()
		}

		if d.Reason != "" && d.Reason != s.unavailable[d.Type] {
			s.o.Logger.Warn("device type unavailable: its connector cannot run",
				slog.String("type", d.Type), slog.String("reason", d.Reason))
		}

		s.unavailable[d.Type] = d.Reason
		out = append(out, d)
	}

	return out
}

// versionLines records whether a probe printed a version line.
type versionLines struct {
	mu   sync.Mutex
	seen bool
}

func (v *versionLines) add(line string) {
	if strings.Contains(strings.ToLower(line), "version") {
		v.mu.Lock()
		v.seen = true
		v.mu.Unlock()
	}
}

func (v *versionLines) printed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.seen
}

func (s *Sources) probe(ctx context.Context, id string, t domain.DeviceType) error {
	path, err := s.tool(t)
	if err != nil {
		return err
	}

	if s.o.Supervisor == nil {
		return errors.New("node.runtime_dir is not set: no tool can run")
	}

	// The connectors may exit non-zero after printing their version: the
	// probe succeeds when a version line was printed, on stdout or stderr.
	var v versionLines

	in, err := s.o.Supervisor.NewInstance(process.Spec{
		ID: id, Kind: "probe", Mode: process.Batch, Probe: true, Path: path, Args: []string{"--version"},
		ToolDirs: s.o.Tools.Dirs, Timeouts: process.Timeouts{Job: ProbeTimeout, Stop: time.Second},
		Stdout: func(_ context.Context, r io.Reader) error {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				v.add(sc.Text())
			}

			return sc.Err()
		},
		OnLine: func(l process.Line) { v.add(l.Text) },
	})
	if err != nil {
		return err
	}

	if err := in.Run(ctx); err != nil {
		return err
	}

	if !v.printed() {
		return fmt.Errorf("%s --version printed no version", filepath.Base(path))
	}

	return nil
}

// New implements app.Sources.
func (s *Sources) New(p domain.DeviceParams) (app.Source, error) {
	if !p.Type.Supported() {
		return nil, fmt.Errorf("device type %s: %w", p.Type, app.ErrSourceUnavailable)
	}

	return &source{s: s, p: p, log: s.o.Logger.With(slog.String("device_id", p.ID.String()))}, nil
}

// source is the connector of one device.
type source struct {
	s   *Sources
	p   domain.DeviceParams
	log *slog.Logger

	mu     sync.Mutex
	center int64
	ctl    net.Conn
}

// Run implements app.Source.
func (src *source) Run(ctx context.Context, t domain.Tuning, sink app.IQSink, report func(app.SourceEvent)) error {
	path, err := src.s.tool(src.p.Type)
	if err != nil {
		report(app.SourceEvent{State: domain.StateUnavailable, Reason: "tool_missing"})

		return fmt.Errorf("%w: %w", app.ErrSourceUnavailable, err)
	}

	src.mu.Lock()
	src.center = t.Center().Hz()
	src.mu.Unlock()

	policy := process.DevicePolicy()
	if src.s.o.Policy != nil {
		policy = *src.s.o.Policy
	}

	core := uint64(0)
	in, err := src.s.o.Supervisor.NewInstance(process.Spec{
		ID: instanceID("dev", src.p.ID.String()), Kind: "connector", Path: path, ToolDirs: src.s.o.Tools.Dirs,
		TouchOnly:   true,
		PerRun:      func() (process.Run, error) { return src.perRun(t.Rate().PerSecond(), sink) },
		StderrRules: connectorRules,
		Sink:        func(e process.Event) { src.event(e, report) },
		Timeouts: process.Timeouts{
			Start: src.s.o.StartTimeout, IdleOutput: src.s.o.StallTimeout, IdleStrikes: 1, Stop: StopGrace,
		},
		Restart: policy,
		Limits:  process.Limits{OpenFiles: 1024, Core: &core, NoNewPrivs: true},
	})
	if err != nil {
		return err
	}

	err = in.Run(ctx)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, process.ErrFailed):
		return fmt.Errorf("%w: %w", app.ErrSourceFailed, err)
	case errors.Is(err, process.ErrUnavailable):
		return fmt.Errorf("%w: %w", app.ErrSourceUnavailable, err)
	default:
		return fmt.Errorf("device %s connector: %w", src.p.ID, err)
	}
}

// event maps a supervisor event to the device lifecycle (§8.2).
func (src *source) event(e process.Event, report func(app.SourceEvent)) {
	ev := app.SourceEvent{Reason: e.Reason, Attempt: e.Attempt}

	switch e.State {
	case process.StateStarting:
		ev.State = domain.StateStarting
	case process.StateRunning:
		if e.Diag != process.DiagNone {
			return
		}

		ev.State = domain.StateRunning
	case process.StateRetryWait:
		ev.State = domain.StateRetryWait
		if e.Reason == "no_tool_output" {
			ev.Reason = "sample_stall"
		}
	case process.StateStopping:
		ev.State = domain.StateStopping
	case process.StateStopped:
		ev.State = domain.StateStopped
	case process.StateFailed:
		ev.State = domain.StateFailed
	case process.StateUnavailable:
		ev.State = domain.StateUnavailable
	default:
		return
	}

	report(ev)
}

// args builds the connector argv (§8.2 rule 2: typed values only).
func (src *source) args(iqPort, ctlPort, rate int) []string {
	src.mu.Lock()
	center := src.center
	src.mu.Unlock()

	d := src.p.Driver
	args := []string{
		"-d", d.Device(),
		"-p", strconv.Itoa(iqPort),
		"-c", strconv.Itoa(ctlPort),
		"-f", strconv.FormatInt(d.HardwareHz(center), 10),
		"-s", strconv.Itoa(rate),
		"-g", d.Gain().String(),
		"-P", strconv.Itoa(d.PPM()),
	}

	if d.IQSwap() {
		args = append(args, "-i")
	}

	if d.BiasTee() {
		args = append(args, "-b")
	}

	if ds := d.DirectSampling(); ds != domain.DirectSamplingOff {
		args = append(args, "-e", strconv.Itoa(int(ds)))
	}

	return args
}

func (src *source) perRun(rate int, sink app.IQSink) (process.Run, error) {
	ports, err := src.s.o.Ports.Take(2)
	if err != nil {
		return process.Run{}, err
	}

	iqAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))
	ctlAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[1]))

	return process.Run{
		Args: src.args(ports[0], ports[1], rate),
		Attach: func(ctx context.Context, touch func()) error {
			go src.control(ctx, ctlAddr)

			return readIQ(ctx, iqAddr, rate, sink, touch)
		},
		Done: func() { src.s.o.Ports.Release(ports) },
	}, nil
}

// control keeps the control connection of the current run.
func (src *source) control(ctx context.Context, addr string) {
	conn, err := dial(ctx, addr)
	if err != nil {
		return
	}

	src.mu.Lock()
	src.ctl = conn
	src.mu.Unlock()

	<-ctx.Done()

	src.mu.Lock()
	if src.ctl == conn {
		src.ctl = nil
	}
	src.mu.Unlock()

	_ = conn.Close()
}

// SetCenter implements app.Source: center_freq (plus the driver
// lfo_offset) over the control socket. The next start uses the new centre
// as well.
func (src *source) SetCenter(hz int64) error {
	src.mu.Lock()
	defer src.mu.Unlock()

	src.center = hz

	if src.ctl == nil {
		return nil
	}

	_ = src.ctl.SetWriteDeadline(time.Now().Add(time.Second))

	if _, err := io.WriteString(src.ctl, "center_freq:"+strconv.FormatInt(src.p.Driver.HardwareHz(hz), 10)+"\n"); err != nil {
		return fmt.Errorf("connector control: %w", err)
	}

	return nil
}

// dial connects to a loopback socket of a starting connector, retrying
// until ctx ends.
func dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer

	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			return conn, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(dialInterval):
		}
	}
}

// readIQ reads CF32 from the connector IQ socket into sink. Readiness (and
// the stall watchdog) is fed once rate/10 samples have arrived (§8.2
// start rule).
func readIQ(ctx context.Context, addr string, rate int, sink app.IQSink, touch func()) error {
	conn, err := dial(ctx, addr)
	if err != nil {
		return err
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	buf := make([]byte, MaxBlock*8)
	iq := make([]complex64, MaxBlock)
	have := 0
	threshold := uint64(rate / 10)

	var index uint64

	for {
		n, err := conn.Read(buf[have:])
		have += n

		if k := have / 8; k > 0 {
			for i := range k {
				re := math.Float32frombits(binary.LittleEndian.Uint32(buf[8*i:]))
				im := math.Float32frombits(binary.LittleEndian.Uint32(buf[8*i+4:]))
				iq[i] = complex(re, im)
			}

			at := time.Now().Add(-time.Duration(k) * time.Second / time.Duration(rate))
			sink.Samples(index, at, iq[:k])
			index += uint64(k)

			if index >= threshold {
				touch()
			}

			rest := have - 8*k
			copy(buf, buf[8*k:have])
			have = rest
		}

		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("iq socket: %w", err)
		}
	}
}
