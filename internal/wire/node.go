package wire

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/agent"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/media"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/grid/infra/probe"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	radioapp "github.com/yohang/mesh-sdr/internal/radio/app"
	radiodomain "github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	"github.com/yohang/mesh-sdr/internal/radio/infra/connector"
	"github.com/yohang/mesh-sdr/internal/radio/infra/decoder"
	"github.com/yohang/mesh-sdr/internal/radio/infra/devlog"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	"github.com/yohang/mesh-sdr/internal/version"
)

// NodeOption customises the node graph (tests).
type NodeOption func(*nodeOptions)

type nodeOptions struct {
	prober         agent.Prober
	mediaHeartbeat time.Duration
	// outbox is told the file outbox (tests send files through it).
	outbox        func(*agent.Outbox)
	probeDecoders bool
}

// WithDecoderProbe adds the decoder capabilities of the node to the
// reports of a prober set by WithProber (tests that run decoders).
func WithDecoderProbe() NodeOption { return func(o *nodeOptions) { o.probeDecoders = true } }

// decoderProber adds the decoder capabilities to a prober's reports.
type decoderProber struct {
	agent.Prober

	decoders func(context.Context) []ctl.Decoder
}

func (p decoderProber) Capabilities(ctx context.Context) ctl.Capabilities {
	c := p.Prober.Capabilities(ctx)
	c.Decoders = append(c.Decoders, p.decoders(ctx)...)

	return c
}

// decoderSettings are the decoding settings of the desired state. The
// enabled FST4, FST4W, Q65 and JS8 lists it lacks (no state yet, a hub
// that predates them) take the defaults of the hub settings; the other
// zero values take the node defaults (decoder.Settings).
func decoderSettings(d *ctl.StateDecoders, def config.SettingsDecoders) decoder.Settings {
	s := decoder.Settings{
		FST4Intervals: seconds(def.FST4Intervals), FST4WIntervals: seconds(def.FST4WIntervals),
		Q65Combinations: def.Q65Combinations, JS8Profiles: def.JS8Profiles, DSCShowErrors: def.DSCShowErrors,
	}

	if d == nil {
		return s
	}

	s.MaxRestarts, s.FFTSize, s.ShowCW, s.DSCShowErrors = d.MaxRestarts, d.DigimodesFFTSize, d.ShowCW, d.DSCShowErrors
	s.WSJTDepth, s.WSJTDepths, s.JS8Depth = d.WSJTDepth, d.WSJTDepths, d.JS8Depth
	s.PagingFilter, s.PagingCharset, s.ISMReportLevels = d.PagingFilter, d.PagingCharset, d.ISMReportLevels

	if f := d.FAX; f != nil {
		s.FAX = decoder.FAXSettings{LPM: f.LPM, MinLength: f.MinLength, MaxLength: f.MaxLength, PostProcess: f.PostProcess, Color: f.Color, AM: f.AM}
	}

	if len(d.FST4Intervals) > 0 {
		s.FST4Intervals = d.FST4Intervals
	}

	if len(d.FST4WIntervals) > 0 {
		s.FST4WIntervals = d.FST4WIntervals
	}

	if len(d.Q65Combinations) > 0 {
		s.Q65Combinations = d.Q65Combinations
	}

	if len(d.JS8Profiles) > 0 {
		s.JS8Profiles = d.JS8Profiles
	}

	return s
}

// queueProber adds the depth of the batch decoder queue to the heartbeats
// (DEC-025).
type queueProber struct {
	agent.Prober

	depth func() int
}

func (p queueProber) Heartbeat(ctx context.Context) ctl.Heartbeat {
	hb := p.Prober.Heartbeat(ctx)
	hb.QueueDepth = p.depth()

	return hb
}

// WithProber replaces the host prober.
func WithProber(p agent.Prober) NodeOption { return func(o *nodeOptions) { o.prober = p } }

// WithMediaHeartbeat sets the connection.heartbeat period of media
// sessions.
func WithMediaHeartbeat(d time.Duration) NodeOption {
	return func(o *nodeOptions) { o.mediaHeartbeat = d }
}

// NodeEnrolled reports whether the node has its certificate (tls.cert).
func NodeEnrolled(cfg config.Node) bool {
	if cfg.TLS.Cert == "" {
		return false
	}

	_, err := os.Stat(cfg.TLS.Cert)

	return err == nil
}

// enrolledNode builds an enrolled node: the mTLS node API with /control.
func enrolledNode(cfg config.Node, id griddomain.NodeID, logger *slog.Logger, opts ...NodeOption) (*Process, error) {
	var o nodeOptions
	for _, opt := range opts {
		opt(&o)
	}

	cert, err := pki.LoadKeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("tls.cert / tls.key: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.HubTrust.CACert)
	if err != nil {
		return nil, fmt.Errorf("hub_trust.ca_cert: %w", err)
	}

	ca, err := pki.ParseCACert(caPEM)
	if err != nil {
		return nil, fmt.Errorf("hub_trust.ca_cert: %w", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)

	key, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("tls.key: unsupported key type")
	}

	devices := DevicesOf(cfg)

	// The capability report probes the drivers and the decoder tools of the
	// radio built below.
	var (
		sources *connector.Sources
		toolbox *decoder.Toolbox
	)

	if o.prober == nil {
		drivers := func(ctx context.Context) []ctl.SDRDriver {
			out := []ctl.SDRDriver{}
			for _, d := range sources.Drivers(ctx) {
				out = append(out, ctl.SDRDriver{Type: d.Type, Available: d.Available, Reason: d.Reason})
			}

			return out
		}
		decoders := func(ctx context.Context) []ctl.Decoder { return decoderCapabilities(ctx, toolbox) }
		o.prober = probe.New(version.String(), func() []ctl.Device { return devices }, drivers, decoders, time.Now())
	} else if o.probeDecoders {
		o.prober = decoderProber{Prober: o.prober, decoders: func(ctx context.Context) []ctl.Decoder { return decoderCapabilities(ctx, toolbox) }}
	}

	// The batch decoder queue of the node (DEC-025): its depth goes with
	// the heartbeat.
	queue := decoder.NewQueue(cfg.Decoders.BatchWorkerCount(runtime.NumCPU()), cfg.Decoders.QueueLength, time.Now,
		component(logger, "radio.infra.decoder.queue"))
	o.prober = queueProber{Prober: o.prober, depth: queue.Depth}

	ag, err := agent.New(agent.Options{
		NodeID: id.String(), Version: version.String(),
		Buffer: agent.NewBuffer(cfg.Node.EventBuffer.MaxEvents, int(cfg.Node.EventBuffer.MaxBytes.Bytes())),
		Prober: o.prober, Now: time.Now, Logger: component(logger, "grid.infra.agent"),
	})
	if err != nil {
		return nil, err
	}

	// The files the decoders produce go to the hub through the outbox
	// (FIL-005), under the node's runtime directory.
	outbox := agent.NewOutbox(filepath.Join(cfg.Node.RuntimeDir, "outbox"), ag, time.Now, component(logger, "grid.infra.agent.outbox"))
	if o.outbox != nil {
		o.outbox(outbox)
	}

	holder := pki.NewCertHolder(cert)
	revoked := pki.NewRevokedSet()

	// The desired state pushed by the hub carries the presets, the WFM
	// de-emphasis and waterfall defaults the radio applies and the listen
	// policy the media server enforces.
	state := agent.NewDesiredState(devices)

	// The device logs (SRC-005): connector lines and lifecycle records,
	// pushed to the hub over the control channel.
	deviceLog := devlog.New(slices.Collect(maps.Keys(cfg.Devices)), devlog.DefaultSize, time.Now)

	// Decoders: decoded messages and images go to the hub, the decoding
	// settings come with the desired state, and a missing tool re-probes
	// the node.
	defaults := config.DefaultSettings().Decoders
	dec := radioDecoding{
		publisher: decodePublisher{ag: ag},
		files: filePublisher{
			outbox: outbox, dir: filepath.Join(cfg.Node.RuntimeDir, "produced"), logger: component(logger, "wire.file_publisher"),
		},
		settings: func() decoder.Settings { return decoderSettings(state.Policy().Decoders, defaults) },
		reprobe:  (&coalesced{run: func() { ag.EmitCapabilities(context.Background()) }}).trigger,
		queue:    queue,
		// Slot decoders need the node clock synchronised and within the
		// offset beyond which the hub marks the node degraded (DEC-026,
		// §4.5).
		clockSynced: func() bool {
			off, limit := ag.ClockOffsetMS(), gridapp.MaxClockOffset.Milliseconds()

			return o.prober.NTPSynced() && off >= -limit && off <= limit
		},
	}

	manager, streams, sources, toolbox, err := newRadio(cfg, logger, deviceReporter{ag: ag, log: deviceLog}, state, deviceLog, dec)
	if err != nil {
		return nil, err
	}

	mediaServer := media.NewServer(media.Options{
		NodeID: id.String(), Version: version.String(), GatewayIdentity: cfg.HubTrust.HubIdentity,
		OwnSerial: holder.Serial, Agent: ag, HeartbeatInterval: o.mediaHeartbeat, Streams: streams, Policy: state,
		Now: time.Now, Logger: component(logger, "grid.infra.media"),
	})

	ctlServer := control.NewNodeServer(control.NodeOptions{
		Agent: ag, State: appliedState{state, streams}, Media: mediaServer, Logs: deviceLog, HubIdentity: cfg.HubTrust.HubIdentity, Revoked: revoked,
		Renewer: &control.FileRenewer{NodeID: id.String(), CertFile: cfg.TLS.Cert, Roots: roots, Key: key, Holder: holder, Now: time.Now},
		Now:     time.Now, Logger: component(logger, "grid.infra.control"),
	})

	srv := httpserver.NewServer(cfg.Node.Listen, gridhttp.NewNodeRouter(ctlServer, mediaServer, component(logger, "grid.http.node")))
	srv.ConnContext = httpserver.NotSentLowat(int(cfg.Node.WSNotSentLowat.Bytes()))
	srv.TLSConfig = pki.NodeServerConfig(holder, roots, revoked)
	// Refused handshakes (foreign CA, revoked peer) are expected noise.
	srv.ErrorLog = slog.NewLogLogger(component(logger, "grid.http.server").Handler(), slog.LevelDebug)

	return &Process{
		addr: cfg.Node.Listen, server: srv, logger: component(logger, "grid.http.server"),
		workers: []func(context.Context){ag.Run, ctlServer.Run, mediaServer.Run, manager.Run, queue.Run},
	}, nil
}

// appliedState tells the stream handler about every applied desired state:
// an edited or unassigned preset is no longer active on its device.
type appliedState struct {
	*agent.DesiredState

	streams *radiohttp.Streams
}

func (s appliedState) Apply(st ctl.StateApply) ctl.StateApplied {
	out := s.DesiredState.Apply(st)
	s.streams.StateApplied()

	return out
}

// deviceReporter sends device states to the hub (device.state, coalesced
// per device in the event buffer, ADR 0008) and records their changes in
// the device log.
type deviceReporter struct {
	ag  *agent.Agent
	log *devlog.Log
}

func (r deviceReporter) DeviceState(s radiodomain.Snapshot) {
	r.log.State(s.ID, string(s.State), s.Reason)

	var center, rate *int64

	// A device with an invalid configuration has no tuning.
	if s.RateHz > 0 {
		c, r := s.CenterHz, int64(s.RateHz)
		center, rate = &c, &r
	}

	r.ag.Emit(rxv1.TypeDeviceState, "device:"+s.ID, agent.ClassState, func(seq int64) any {
		return ctl.DeviceState{
			Seq: seq, DeviceID: s.ID, State: string(s.State), Reason: s.Reason, ActivePresetID: s.ActivePreset,
			CenterFreq: center, SampleRate: rate, Listeners: s.Listeners,
		}
	})
}

// decoderCapabilities is the decoders part of the capability report
// (DEC-001): the analog demodulators, then the probed decoder tools.
func decoderCapabilities(ctx context.Context, toolbox *decoder.Toolbox) []ctl.Decoder {
	out := []ctl.Decoder{}

	for _, c := range engine.Capabilities() {
		out = append(out, ctl.Decoder{Cap: c.Cap, Tools: []ctl.Tool{}, Modes: c.Modes})
	}

	if toolbox == nil {
		return out
	}

	for _, c := range toolbox.Probe(ctx) {
		d := ctl.Decoder{Cap: c.Cap, Tools: make([]ctl.Tool, 0, len(c.Tools)), Modes: c.Modes}
		for _, t := range c.Tools {
			d.Tools = append(d.Tools, ctl.Tool{Name: t.Name, Version: t.Version, OK: t.OK, Reason: t.Reason})
		}

		out = append(out, d)
	}

	return out
}

// coalesced runs run in the background, at most once at a time: the
// triggers that come while it runs make one more run (several decoder
// sessions losing their tool re-probe the node once).
type coalesced struct {
	run func()

	mu      sync.Mutex
	running bool
	dirty   bool
}

func (c *coalesced) trigger() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		c.dirty = true

		return
	}

	c.running = true

	go func() {
		for {
			c.run()

			c.mu.Lock()
			if !c.dirty {
				c.running = false
				c.mu.Unlock()

				return
			}

			c.dirty = false
			c.mu.Unlock()
		}
	}()
}

// decodePublisher sends decoded messages to the hub (decode.batch, one
// message per event; ClassDecode in the event buffer).
type decodePublisher struct{ ag *agent.Agent }

func (p decodePublisher) Decoded(d radioapp.Decoded) {
	source := ctl.SourceListener
	if d.CID == "" {
		source = ctl.SourceBackground
	}

	p.ag.Emit(rxv1.TypeDecodeBatch, "", agent.ClassDecode, func(seq int64) any {
		return ctl.DecodeBatch{Seq: seq, Decodes: []ctl.Decode{{
			DeviceID: d.DeviceID, SessionID: d.SessionID, PresetID: d.PresetID, Mode: d.Mode, Family: d.Family, Freq: d.FreqHz,
			TS: d.Time.UnixMilli(), Source: source, CID: d.CID, Schema: d.Schema, Text: d.Text, Payload: d.Payload,
		}}}
	})
}

// filePublisher sends the images of the decoders to the hub through the
// file outbox (FIL-005): each is written under dir (in node.runtime_dir),
// then moved to the outbox, in the background.
type filePublisher struct {
	outbox *agent.Outbox
	dir    string
	logger *slog.Logger
}

func (p filePublisher) Produced(f radioapp.ProducedFile) {
	go func() {
		meta := agent.FileMeta{
			Kind: f.Kind, DeviceID: f.DeviceID, PresetID: f.PresetID, DecoderSessionID: f.SessionID, Mode: f.Mode,
			FrequencyHz: f.FreqHz, ReceivedStart: f.Start, ReceivedEnd: f.End, Metadata: f.Metadata,
		}

		err := p.send(meta, f.Data)
		if err != nil {
			p.logger.WarnContext(context.Background(), "decoded file not sent to the hub", slog.String("kind", f.Kind), slog.String("device_id", f.DeviceID),
				slog.Int("bytes", len(f.Data)), slog.Any("error", err))

			return
		}

		p.logger.DebugContext(context.Background(), "decoded file queued for the hub", slog.String("kind", f.Kind), slog.String("device_id", f.DeviceID),
			slog.Int("bytes", len(f.Data)))
	}()
}

func (p filePublisher) send(meta agent.FileMeta, data []byte) error {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return fmt.Errorf("create the produced files directory: %w", err)
	}

	tmp, err := os.CreateTemp(p.dir, meta.Kind+"-*.png")
	if err != nil {
		return fmt.Errorf("write produced file: %w", err)
	}

	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(tmp.Name())

		return fmt.Errorf("write produced file: %w", err)
	}

	return p.outbox.SendFile(context.Background(), meta, tmp.Name())
}

// Enrollment is the one-off process of `meshsdr node enroll`.
type Enrollment struct {
	*Process

	Enroller *enroll.NodeEnroller
	Key      *ecdsa.PrivateKey
	Paths    enroll.Paths
}

// NodeEnrollment builds the enrollment process of a node: POST /enroll on
// node.listen with a self-signed certificate of a fresh key (§4.2 step 3).
func NodeEnrollment(cfg config.Node, logger *slog.Logger, token griddomain.EnrollmentToken, caFingerprint [32]byte, now func() time.Time) (*Enrollment, error) {
	id, err := griddomain.NewNodeID(cfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	key, self, err := selfSigned(cfg, id, now())
	if err != nil {
		return nil, err
	}

	e := enroll.NewNodeEnroller(enroll.NodeOptions{
		ID: id, Listen: cfg.Node.Listen, Key: key, SelfSigned: self, Token: token,
		CAFingerprint: caFingerprint, Now: now, Logger: component(logger, "grid.infra.enroll"),
	})

	return &Enrollment{
		Process:  preEnrollment(cfg.Node.Listen, self, e.ServeEnroll, logger),
		Enroller: e,
		Key:      key,
		Paths:    enroll.Paths{Key: cfg.TLS.Key, Cert: cfg.TLS.Cert, CA: cfg.HubTrust.CACert},
	}, nil
}

// selfSigned returns a fresh node key and its self-signed certificate, the
// identity of a node before its enrollment (§4.2 step 3).
func selfSigned(cfg config.Node, id griddomain.NodeID, now time.Time) (*ecdsa.PrivateKey, tls.Certificate, error) {
	key, err := pki.GenerateKey()
	if err != nil {
		return nil, tls.Certificate{}, err
	}

	cert, err := pki.SelfSigned(key, id.String(), cfg.Node.Listen, now)
	if err != nil {
		return nil, tls.Certificate{}, err
	}

	return key, cert, nil
}

// preEnrollment is the process of a node that is not enrolled: the
// pre-enrollment API (enroll serves POST /enroll, nil answers 501) on
// listen, over TLS 1.3 with the self-signed certificate cert.
func preEnrollment(listen string, cert tls.Certificate, enroll http.HandlerFunc, logger *slog.Logger) *Process {
	srv := httpserver.NewServer(listen, gridhttp.NewPreEnrollmentRouter(enroll, component(logger, "grid.http.enrollment")))
	// Refused handshakes are expected noise.
	srv.ErrorLog = slog.NewLogLogger(component(logger, "grid.http.server").Handler(), slog.LevelDebug)
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}

	return &Process{addr: listen, server: srv, logger: component(logger, "grid.http.server")}
}

// DevicesOf lists the [devices.<id>] of the node config, ordered by id
// (TOML tables carry no order once decoded).
func DevicesOf(cfg config.Node) []ctl.Device {
	out := make([]ctl.Device, 0, len(cfg.Devices))

	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		d := cfg.Devices[id]
		out = append(out, ctl.Device{
			ID: id, Name: d.Name, Type: d.Type, Enabled: d.Enabled == nil || *d.Enabled,
			FreqMin: d.FreqRange.Min.Hz(), FreqMax: d.FreqRange.Max.Hz(), SampleRates: slices.Clone(d.SampleRates),
			ListenPolicy: d.ListenPolicy, OperatorCanRetune: d.OperatorCanRetune, AlwaysOn: d.AlwaysOn, SchedulerEnabled: d.SchedulerEnabled,
			Config: deviceConfigOf(d.Driver),
		})
	}

	return out
}

// deviceConfigOf reports the driver values of a device (SRC-022): the hub
// shows them read-only.
func deviceConfigOf(d config.Driver) *ctl.DeviceConfig {
	ds := d.DirectSampling
	if ds == "" {
		ds = "off"
	}

	return &ctl.DeviceConfig{
		RFGain: d.RFGain.String(), PPM: d.PPM, BiasTee: d.BiasTee, DirectSampling: ds, IQSwap: d.IQSwap, LFOOffset: d.LFOOffset,
	}
}
