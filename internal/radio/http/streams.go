// Package http serves the device messages of the node media WebSocket
// (TECHNICAL_SPEC §6.4, §6.5): device.attach / detach, stream.configure,
// audio.configure, demod.create / set / remove, device.retune and
// preset.select (shared centre: one tuning for every listener). The
// grid media endpoint authenticates the connection, checks the token scope
// and hands these messages over (media.Streams); this package turns them
// into device manager and DSP engine calls, and the engine output into
// rx.v1 frames on the connection's send queue.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"
)

// Device defaults sent in device.config (FEATURE_SPEC defaults of
// waterfall.*, dsp.squelch_auto_margin; ADR 0019). The waterfall levels and
// palette are replaced by the hub settings once the desired state carries
// them (ADR 0026).
const (
	tuningStepHz     = 1000
	waterfallMin     = -88
	waterfallMax     = -20
	autoMinRange     = 50
	waterfallScheme  = "turbo"
	squelchMargin    = 10
	squelchInitial   = -150
	defaultMaxDemods = 1
)

// Devices is the device manager seen by the handler.
type Devices interface {
	Attach(id string) (*app.Lease, error)
	Retune(id string, hz, rate int64) (domain.Snapshot, error)
	SetActivePreset(id, preset string) error
	Watch(id string, fn func(domain.Snapshot)) (func(), error)
}

// DesiredState is the desired state pushed by the hub
// (grid/agent.DesiredState): the presets of each device and the global
// settings.
type DesiredState interface {
	Device(id string) (ctl.DesiredDevice, bool)
	Preset(id string) (ctl.Preset, bool)
	Policy() ctl.StatePolicy
}

// Streams implements media.Streams.
type Streams struct {
	devices Devices
	state   DesiredState
	log     *slog.Logger

	// switching serialises the shared changes of the devices
	// (preset.select, device.retune) and guards lastSwitch, applied and
	// waterfall.
	switching sync.Mutex
	// lastSwitch is the time of the last preset switch of each device
	// (§5.11 limit).
	lastSwitch map[string]time.Time
	// applied is the active preset of each device as it was switched: an
	// edit or an unassignment by the hub clears it.
	applied map[string]activePreset
	// waterfall are the waterfall defaults the listeners last received.
	waterfall media.Waterfall
	// retunes limits the device.retune of each device (§5.11).
	retunes *ratelimit.Limiter[string]
	now     func() time.Time

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// Per-device limits of the shared changes (§5.11): one preset switch per
// 5 s, four retunes per second.
const (
	PresetSwitchEvery = 5 * time.Second
	RetuneEvery       = 250 * time.Millisecond
	RetuneBurst       = 4
)

// NewStreams returns the handler. state may be nil (no hub state: no
// preset, the node defaults apply).
func NewStreams(d Devices, state DesiredState, log *slog.Logger) *Streams {
	s := &Streams{
		devices: d, state: state, log: log, sessions: map[*session]struct{}{},
		lastSwitch: map[string]time.Time{}, applied: map[string]activePreset{},
		retunes: ratelimit.New[string](RetuneEvery, RetuneBurst, ratelimit.DefaultCapacity), now: time.Now,
	}
	s.waterfall = s.waterfallDefaults()

	return s
}

// presets returns what device.config shows of the presets of a device: the
// active preset activeID (nil when none or no longer offered) and the
// presets it may switch to.
func (s *Streams) presets(device, activeID string) (*media.PresetRef, *ctl.Preset, []media.PresetRef) {
	avail := []media.PresetRef{}

	if s.state == nil {
		return nil, nil, avail
	}

	want, ok := s.state.Device(device)
	if !ok {
		return nil, nil, avail
	}

	var (
		ref    *media.PresetRef
		active *ctl.Preset
	)

	for _, id := range want.Presets {
		p, ok := s.state.Preset(id)
		if !ok {
			continue
		}

		avail = append(avail, media.PresetRef{ID: id, Name: p.Name})

		if id == activeID {
			ref, active = &media.PresetRef{ID: id, Name: p.Name}, &p
		}
	}

	return ref, active, avail
}

// activePreset is a switched preset: its id and its content then.
type activePreset struct {
	id string
	p  ctl.Preset
}

// StateApplied clears the active preset of the devices whose preset the
// hub edited or no longer assigns to them, and sends new waterfall defaults
// to every listener (device.config.patch). Call it after every applied
// desired state.
func (s *Streams) StateApplied() {
	s.switching.Lock()
	defer s.switching.Unlock()

	if w := s.waterfallDefaults(); w != s.waterfall {
		s.waterfall = w

		s.mu.Lock()
		all := slices.Collect(maps.Keys(s.sessions))
		s.mu.Unlock()

		for _, ss := range all {
			ss.waterfallChanged(w)
		}
	}

	for device, was := range s.applied {
		if p, err := s.resolve(device, was.id); err == nil && reflect.DeepEqual(p, was.p) {
			continue
		}

		s.clearPreset(device)
	}
}

// clearPreset forgets the active preset of a device (switching held).
func (s *Streams) clearPreset(device string) {
	delete(s.applied, device)

	if err := s.devices.SetActivePreset(device, ""); err != nil {
		s.log.Warn("active preset not cleared", slog.String("device_id", device), slog.Any("error", err))
	}
}

// resolve returns a preset of the desired state that the device may run.
func (s *Streams) resolve(device, id string) (ctl.Preset, error) {
	if s.state == nil {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodeNotFound, Reason: "no preset received from the hub"}
	}

	p, ok := s.state.Preset(id)
	if !ok {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodeNotFound, Reason: "unknown preset"}
	}

	if want, ok := s.state.Device(device); !ok || !slices.Contains(want.Presets, id) {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodePresetIncompatible, Reason: "the preset does not fit this device"}
	}

	return p, nil
}

// listeners returns the sessions attached to a device.
func (s *Streams) listeners(device string) []*session {
	s.mu.Lock()
	all := slices.Collect(maps.Keys(s.sessions))
	s.mu.Unlock()

	out := all[:0]

	for _, ss := range all {
		if ss.attachedTo(device) != nil {
			out = append(out, ss)
		}
	}

	return out
}

// waterfallDefaults returns the waterfall defaults of device.config: the
// hub settings when the desired state carries them, else the node defaults.
func (s *Streams) waterfallDefaults() media.Waterfall {
	w := media.Waterfall{Levels: media.Levels{Min: waterfallMin, Max: waterfallMax}, AutoMinRange: autoMinRange, Scheme: waterfallScheme}

	if s.state == nil {
		return w
	}

	if p := s.state.Policy().Waterfall; p != nil {
		w.Levels, w.Scheme = media.Levels{Min: float64(p.MinDB), Max: float64(p.MaxDB)}, p.Palette
	}

	return w
}

// Open implements media.Streams.
func (s *Streams) Open(p media.Peer) media.StreamSession {
	ss := &session{
		s: s, peer: p, q: p.Queue(), next: 1,
		devices: map[string]*attached{}, demods: map[string]*demodState{},
		audio: audioConfig{codec: preferredCodec(p.Hello()), rate: dsp.DefaultOutputRate},
	}

	s.mu.Lock()
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()

	return ss
}

type audioConfig struct {
	codec app.AudioCodec
	rate  int
}

// preferredCodec picks IMA ADPCM unless the client only takes PCM; Opus is
// not provided by this node (ADR 0019).
func preferredCodec(h media.Hello) app.AudioCodec {
	codecs := h.Capabilities.AudioCodecs
	if len(codecs) > 0 && !slices.Contains(codecs, media.CodecADPCM) && slices.Contains(codecs, media.CodecPCM) {
		return app.CodecPCM
	}

	return app.CodecADPCM
}

type attached struct {
	id       string
	lease    *app.Lease
	stream   uint16
	fps      int
	paused   bool
	unsub    func()
	unwatch  func()
	revision int
	center   int64
	rate     int
	state    domain.State
}

type demodState struct {
	id     string
	device string
	stream uint16
	demod  app.Demod
	// mu makes each read-modify-write of the parameters atomic: the
	// listener's demod.set and audio.configure, and the moves of a shared
	// centre change made by another session.
	mu sync.Mutex
}

func wireCodec(c app.AudioCodec) rxv1.Codec {
	if c == app.CodecPCM {
		return rxv1.CodecPCMS16LE
	}

	return rxv1.CodecADPCMIMA
}

type session struct {
	s    *Streams
	peer media.Peer
	q    *sendq.Queue

	mu      sync.Mutex
	next    uint16
	nextID  int
	devices map[string]*attached
	demods  map[string]*demodState
	audio   audioConfig
	closed  bool
	// reported is the device and mode last reported to the peer (report).
	reported [2]string
}

func (ss *session) streamID() uint16 {
	id := ss.next
	ss.next++

	if ss.next == 0 {
		ss.next = 1
	}

	return id
}

// Handle implements media.StreamSession.
func (ss *session) Handle(_ context.Context, req rxv1.Envelope) {
	switch req.Type() {
	case rxv1.TypeDeviceAttach:
		ss.attach(req)
	case rxv1.TypeDeviceDetach:
		ss.detach(req)
	case rxv1.TypeStreamConfigure:
		ss.configureStream(req)
	case rxv1.TypeAudioConfigure:
		ss.configureAudio(req)
	case rxv1.TypeDemodCreate:
		ss.createDemod(req)
	case rxv1.TypeDemodSet:
		ss.setDemod(req)
	case rxv1.TypeDemodRemove:
		ss.removeDemod(req)
	case rxv1.TypeDeviceRetune:
		ss.retune(req)
	case rxv1.TypePresetSelect:
		ss.selectPreset(req)
	default:
		ss.peer.Fail(req, rxv1.CodeUnsupportedType, "not a device message")
	}
}

func decode[T any](req rxv1.Envelope) (T, error) {
	var v T

	err := req.DecodePayload(&v, false)

	return v, err
}

// fail answers req with the protocol code of err.
func (ss *session) fail(req rxv1.Envelope, err error) {
	var pe *rxv1.Error
	if errors.As(err, &pe) {
		ss.peer.Fail(req, pe.Code, pe.Reason)

		return
	}

	var de *shared.Error
	if errors.As(err, &de) {
		code := rxv1.CodeInternal

		switch {
		case errors.Is(err, domain.ErrDeviceNotFound):
			code = rxv1.CodeNotFound
		case errors.Is(err, domain.ErrDeviceUnavailable):
			code = rxv1.CodeDeviceUnavailable
		case errors.Is(err, domain.ErrUnsupportedMode):
			code = rxv1.CodeDemodError
		case errors.Is(err, domain.ErrCapacityExceeded):
			code = rxv1.CodeCapacityExceeded
		case de.Kind() == shared.KindInvalid:
			code = rxv1.CodeOutOfRange
		}

		ss.peer.Fail(req, code, de.Message())

		return
	}

	ss.s.log.Error("device message failed", slog.String("type", string(req.Type())), slog.Any("error", err))
	ss.peer.Fail(req, rxv1.CodeInternal, "internal error")
}

// deviceConfig builds device.config at revision rev. The start
// demodulator, tuning step and initial squelch and NR come from the active
// preset, if any.
func (ss *session) deviceConfig(rev int, snap domain.Snapshot, info app.SpectrumInfo) media.DeviceConfig {
	c := ss.peer.Claims()
	perm := c.Allows(snap.ID, token.PermRetune)
	ref, active, avail := ss.s.presets(snap.ID, snap.ActivePreset)

	cfg := media.DeviceConfig{
		DeviceID: snap.ID, Revision: rev, CenterHz: snap.CenterHz, SampleRate: snap.RateHz,
		ActivePreset: ref, PresetsAvailable: avail, TuningStepHz: tuningStepHz,
		Start:     media.Start{Mode: media.ModeNFM},
		Waterfall: ss.s.waterfallDefaults(),
		FFT:       media.FFTConfig{Size: info.Size, FPS: info.FPS},
		Squelch:   media.Squelch{Initial: squelchInitial, AutoMargin: squelchMargin},
		Limits:    media.FreqLimits{MinHz: snap.MinHz, MaxHz: snap.MaxHz},
		Permissions: media.Permissions{
			Preset: c.Allows(snap.ID, token.PermPreset), Retune: perm,
		},
	}

	if p := active; p != nil {
		cfg.Start = media.Start{Mode: p.StartMod}

		// A retune since the switch may leave the start outside the band.
		if off := p.StartFreq - snap.CenterHz; abs(off) <= int64(snap.RateHz)/2 {
			cfg.Start.OffsetHz = off
		}
		cfg.TuningStepHz = int(p.TuningStep)

		if p.InitialSquelchLevel != nil {
			cfg.Squelch.Initial = float64(*p.InitialSquelchLevel)
		}

		if p.InitialNRLevel != nil {
			cfg.NRInitial = *p.InitialNRLevel
		}
	}

	return cfg
}

func fftOf(info app.SpectrumInfo) *media.StreamFFT {
	scale := rxv1.DefaultFFTU8Scale()

	return &media.StreamFFT{Size: info.Size, StartHz: info.StartHz, SpanHz: info.SpanHz, DBMin: scale.DBMin, DBStep: scale.DBStep}
}

func (ss *session) attach(req rxv1.Envelope) {
	p, err := decode[media.DeviceAttach](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	if p.FFT.Codec != "" && p.FFT.Codec != media.CodecFFTU8 {
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "fft codec "+strconv.Quote(p.FFT.Codec)+" is not provided by this node (u8-db)")

		return
	}

	ss.mu.Lock()
	_, dup := ss.devices[p.DeviceID]
	ss.mu.Unlock()

	if dup {
		ss.peer.Fail(req, rxv1.CodeConflict, "device already attached")

		return
	}

	lease, err := ss.s.devices.Attach(p.DeviceID)
	if err != nil {
		ss.fail(req, err)

		return
	}

	eng := lease.Engine()
	info := eng.Spectrum()
	snap := lease.Snapshot()

	fps := info.FPS
	if p.FFT.FPS > 0 {
		fps = min(p.FFT.FPS, info.FPS)
	}

	if m := ss.peer.Hello().Capabilities.MaxFFTFPS; m > 0 {
		fps = min(fps, m)
	}

	ss.mu.Lock()
	a := &attached{id: p.DeviceID, lease: lease, stream: ss.streamID(), fps: fps, center: snap.CenterHz, rate: snap.RateHz, state: snap.State}
	ss.devices[p.DeviceID] = a
	ss.mu.Unlock()

	open := media.StreamOpen{StreamID: a.stream, Kind: media.KindFFT, Codec: media.CodecFFTU8, FFT: fftOf(info), FPS: fps}
	cfg := ss.deviceConfig(0, snap, info)

	ss.peer.Ack(req, media.AttachResult{Device: cfg, Streams: []media.StreamOpen{open}})
	ss.peer.Send(rxv1.TypeDeviceConfig, cfg)
	ss.peer.Send(rxv1.TypeDeviceState, media.DeviceState{DeviceID: snap.ID, State: string(snap.State), Reason: snap.Reason})
	ss.peer.Send(rxv1.TypeStreamOpen, open)
	ss.report()

	ss.q.ConfigureFFT(a.stream, fps, false)

	stream := a.stream
	unsub := eng.SubscribeSpectrum(func(f app.SpectrumFrame) {
		_ = ss.q.PushFFT(sendq.Frame{
			Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, StreamID: stream, TimestampUS: f.TimestampUS, Payload: f.Payload,
		})
	})

	unwatch, err := ss.s.devices.Watch(p.DeviceID, func(s domain.Snapshot) { ss.onState(a, s) })
	if err != nil {
		unwatch = func() {}
	}

	ss.mu.Lock()
	a.unsub, a.unwatch = unsub, unwatch
	ss.mu.Unlock()
}

// onState forwards device changes to the client: device.state, and a
// device.config.patch plus a stream.update after a retune.
func (ss *session) onState(a *attached, s domain.Snapshot) {
	ss.mu.Lock()
	if ss.closed || ss.devices[a.id] != a {
		ss.mu.Unlock()

		return
	}

	stateChanged := s.State != a.state
	retuned := s.CenterHz != a.center || s.RateHz != a.rate
	a.state, a.center, a.rate = s.State, s.CenterHz, s.RateHz

	if retuned {
		a.revision++
	}

	rev := a.revision
	ss.mu.Unlock()

	if stateChanged {
		ss.peer.Send(rxv1.TypeDeviceState, media.DeviceState{DeviceID: s.ID, State: string(s.State), Reason: s.Reason})
	}

	if retuned {
		ss.peer.Send(rxv1.TypeDeviceConfigPatch, media.DeviceConfigPatch{
			DeviceID: s.ID, Revision: rev, Set: map[string]any{"center_hz": s.CenterHz, "sample_rate": s.RateHz}, Unset: []string{},
		})
		ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: a.stream, FFT: fftOf(a.lease.Engine().Spectrum())})
	}
}

// waterfallChanged sends new waterfall defaults for every attached device.
func (ss *session) waterfallChanged(w media.Waterfall) {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()

		return
	}

	patches := make([]media.DeviceConfigPatch, 0, len(ss.devices))

	for id, a := range ss.devices {
		a.revision++
		patches = append(patches, media.DeviceConfigPatch{DeviceID: id, Revision: a.revision, Set: map[string]any{"waterfall": w}, Unset: []string{}})
	}
	ss.mu.Unlock()

	for _, p := range patches {
		ss.peer.Send(rxv1.TypeDeviceConfigPatch, p)
	}
}

// inBand checks an offset against the capture band (§6.9: ±sample_rate/2).
func inBand(a *attached, offset int64) bool {
	half := int64(a.lease.Snapshot().RateHz) / 2

	return offset >= -half && offset <= half
}

func (ss *session) detach(req rxv1.Envelope) {
	p, err := decode[media.DeviceRef](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	ss.mu.Lock()
	a := ss.devices[p.DeviceID]
	delete(ss.devices, p.DeviceID)

	var demods []*demodState

	for id, d := range ss.demods {
		if d.device == p.DeviceID {
			demods = append(demods, d)
			delete(ss.demods, id)
		}
	}
	ss.mu.Unlock()

	if a == nil {
		ss.peer.Fail(req, rxv1.CodeNotFound, "device not attached")

		return
	}

	for _, d := range demods {
		ss.closeDemod(d)
	}

	ss.release(a)
	ss.peer.Ack(req, struct{}{})
	ss.report()
}

func (ss *session) release(a *attached) {
	if a.unsub != nil {
		a.unsub()
	}

	if a.unwatch != nil {
		a.unwatch()
	}

	ss.q.RemoveStream(a.stream)
	ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: a.stream, Reason: media.ReasonClosed})
	a.lease.Release()
}

func (ss *session) configureStream(req rxv1.Envelope) {
	p, err := decode[media.StreamConfigure](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	ss.mu.Lock()

	var a *attached

	for _, d := range ss.devices {
		if d.stream == p.StreamID {
			a = d
		}
	}

	if a == nil {
		ss.mu.Unlock()
		ss.peer.Fail(req, rxv1.CodeNotFound, "no such FFT stream")

		return
	}

	info := a.lease.Engine().Spectrum()

	if p.FPS != nil {
		if *p.FPS < 1 || *p.FPS > info.FPS {
			ss.mu.Unlock()
			ss.peer.Fail(req, rxv1.CodeOutOfRange, "fps: want 1.."+strconv.Itoa(info.FPS))

			return
		}

		a.fps = *p.FPS
	}

	if p.Paused != nil {
		a.paused = *p.Paused
	}

	fps, paused, stream := a.fps, a.paused, a.stream
	ss.mu.Unlock()

	if p.Codec != nil && *p.Codec != media.CodecFFTU8 {
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "fft codec "+strconv.Quote(*p.Codec)+" is not provided by this node")

		return
	}

	ss.q.ConfigureFFT(stream, fps, paused)
	ss.peer.Ack(req, media.StreamResult{Stream: media.StreamOpen{
		StreamID: stream, Kind: media.KindFFT, Codec: media.CodecFFTU8, FFT: fftOf(info), FPS: fps, Paused: paused,
	}})
}

// configureAudio applies audio.configure to the connection and its
// demodulators. pcm-s16le and adpcm-ima are provided; opus is answered
// with adpcm-ima, which every client supports (§6.7 MUST, ADR 0015
// decision 6: the ack names the codec in use); other codecs are refused.
// The change applies to every demodulator or to none.
func (ss *session) configureAudio(req rxv1.Envelope) {
	p, err := decode[media.AudioConfigure](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	if !slices.Contains(dsp.OutputRates, p.SampleRate) {
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "sample_rate: want one of 8000, 11025, 12000, 16000, 22050, 24000, 44100, 48000")

		return
	}

	var codec app.AudioCodec

	switch p.Codec {
	case media.CodecPCM:
		codec = app.CodecPCM
	case media.CodecADPCM, media.CodecOpus:
		codec = app.CodecADPCM
	default:
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "codec: want pcm-s16le, adpcm-ima or opus")

		return
	}

	ss.mu.Lock()
	demods := make([]*demodState, 0, len(ss.demods))

	for _, d := range ss.demods {
		demods = append(demods, d)
	}
	ss.mu.Unlock()

	type change struct {
		d   *demodState
		old app.DemodParams
	}

	var done []change

	for _, d := range demods {
		d.mu.Lock()
		old := d.demod.Params()
		params := old
		params.Codec, params.OutputRate = codec, p.SampleRate
		err := d.demod.Set(params)
		d.mu.Unlock()

		if err != nil {
			for _, c := range done {
				c.d.mu.Lock()
				back := c.d.demod.Params()
				back.Codec, back.OutputRate = c.old.Codec, c.old.OutputRate
				_ = c.d.demod.Set(back)
				c.d.mu.Unlock()
			}

			ss.fail(req, err)

			return
		}

		done = append(done, change{d: d, old: old})
	}

	ss.mu.Lock()
	ss.audio = audioConfig{codec: codec, rate: p.SampleRate}
	ss.mu.Unlock()

	for _, c := range done {
		ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: c.d.stream, Codec: string(codec), SampleRate: p.SampleRate})
	}

	ss.peer.Ack(req, media.AudioConfigure{Codec: string(codec), SampleRate: p.SampleRate})
}

func applied(p app.DemodParams) media.Applied {
	return media.Applied{
		Mode: p.Mode, OffsetHz: p.OffsetHz, Bandpass: media.Bandpass{LowHz: p.LowHz, HighHz: p.HighHz}, SquelchDB: p.SquelchDB,
		NR: media.NR{Enabled: p.NR.Enabled, Threshold: p.NR.ThresholdDB},
	}
}

func (ss *session) createDemod(req rxv1.Envelope) {
	p, err := decode[media.DemodCreate](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	maxDemods := ss.peer.Claims().Limits.MaxDemods
	if maxDemods <= 0 {
		maxDemods = defaultMaxDemods
	}

	ss.mu.Lock()
	a := ss.devices[p.DeviceID]
	count := len(ss.demods)
	audio := ss.audio
	ss.mu.Unlock()

	switch {
	case a == nil:
		ss.peer.Fail(req, rxv1.CodeConflict, "attach the device first")

		return
	case count >= maxDemods:
		ss.peer.Fail(req, rxv1.CodeCapacityExceeded, "demodulator limit reached ("+strconv.Itoa(maxDemods)+")")

		return
	}

	ss.mu.Lock()
	ss.nextID++
	id := "d" + strconv.Itoa(ss.nextID)
	stream := ss.streamID()
	ss.mu.Unlock()

	if !inBand(a, p.OffsetHz) {
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "offset_hz: outside the capture band")

		return
	}

	// Without bandpass (zero edges) the engine applies the mode's default.
	params := app.DemodParams{
		Mode: p.Mode, OffsetHz: p.OffsetHz, SquelchDB: p.SquelchDB, OutputRate: audio.rate, Codec: audio.codec,
	}

	if p.Bandpass != nil {
		params.LowHz, params.HighHz = p.Bandpass.LowHz, p.Bandpass.HighHz
	}

	if p.NR != nil {
		params.NR = app.NR{Enabled: p.NR.Enabled, ThresholdDB: p.NR.Threshold}
	}

	meterKey := "meter:" + id

	d, err := a.lease.NewDemod(params, func(out app.AudioOut) {
		f := sendq.Frame{
			Type: rxv1.FrameAudio, StreamID: stream, TimestampUS: out.TimestampUS,
			Payload: out.Payload, Duration: out.Duration,
		}

		if out.Squelched {
			f.Flags |= rxv1.FlagSquelched
		}

		if out.Reset {
			f.Flags |= rxv1.FlagReset
		}

		if out.Discontinuity {
			f.Flags |= rxv1.FlagDiscontinuity
		}

		// The codec of the framer that produced this frame: frames queued
		// before an audio.configure keep their own label.
		f.Codec = wireCodec(out.Codec)

		_ = ss.q.PushAudio(f)
	}, func(m app.Meter) {
		if b := meterJSON(id, m); b != nil {
			ss.q.PushMeter(meterKey, b)
		}
	})
	if err != nil {
		ss.fail(req, err)

		return
	}

	ss.mu.Lock()
	ss.demods[id] = &demodState{id: id, device: p.DeviceID, stream: stream, demod: d}
	ss.mu.Unlock()

	ss.peer.Send(rxv1.TypeStreamOpen, media.StreamOpen{
		StreamID: stream, Kind: media.KindAudio, Codec: string(audio.codec), SampleRate: audio.rate, Channels: 1, DemodID: id,
	})
	ss.peer.Ack(req, media.DemodCreated{DemodID: id, AudioStreamID: stream, Applied: applied(d.Params())})
	ss.report()
}

func meterJSON(id string, m app.Meter) []byte {
	env, err := rxv1.NewEnvelope(rxv1.TypeDemodMeter, rxv1.CorrelationID{}, time.Now().UnixMilli(), media.DemodMeter{DemodID: id, LevelDB: m.LevelDB, SquelchOpen: m.Open})
	if err != nil {
		return nil
	}

	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}

	return b
}

func (ss *session) demod(req rxv1.Envelope, id string) *demodState {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	d := ss.demods[id]
	if d == nil {
		ss.peer.Fail(req, rxv1.CodeNotFound, "no such demodulator")
	}

	return d
}

func (ss *session) setDemod(req rxv1.Envelope) {
	p, err := decode[media.DemodSet](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	d := ss.demod(req, p.DemodID)
	if d == nil {
		return
	}

	if !ss.peer.Claims().Allows(d.device, token.PermDemod) {
		ss.peer.Fail(req, rxv1.CodeForbidden, "the access token does not grant demod on this device")

		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	params := d.demod.Params()

	// A new mode takes its default pass band unless one is given.
	if p.Mode != nil && *p.Mode != params.Mode {
		params.Mode = *p.Mode
		params.LowHz, params.HighHz = 0, 0
	}

	if p.OffsetHz != nil {
		ss.mu.Lock()
		a := ss.devices[d.device]
		ss.mu.Unlock()

		if a == nil || !inBand(a, *p.OffsetHz) {
			ss.peer.Fail(req, rxv1.CodeOutOfRange, "offset_hz: outside the capture band")

			return
		}

		params.OffsetHz = *p.OffsetHz
	}

	if p.Bandpass != nil {
		params.LowHz, params.HighHz = p.Bandpass.LowHz, p.Bandpass.HighHz
	}

	if p.SquelchDB != nil {
		params.SquelchDB = p.SquelchDB
	}

	if p.NR != nil {
		params.NR = app.NR{Enabled: p.NR.Enabled, ThresholdDB: p.NR.Threshold}
	}

	if err := d.demod.Set(params); err != nil {
		ss.fail(req, err)

		return
	}

	ss.peer.Ack(req, media.AppliedResult{Applied: applied(d.demod.Params())})

	if p.Mode != nil {
		ss.report()
	}
}

func (ss *session) removeDemod(req rxv1.Envelope) {
	p, err := decode[media.DemodRef](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	d := ss.demod(req, p.DemodID)
	if d == nil {
		return
	}

	ss.mu.Lock()
	delete(ss.demods, p.DemodID)
	ss.mu.Unlock()

	ss.closeDemod(d)
	ss.peer.Ack(req, struct{}{})
	ss.report()
}

func (ss *session) closeDemod(d *demodState) {
	d.demod.Close()
	ss.q.RemoveStream(d.stream)
	ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: d.stream, Reason: media.ReasonClosed})
}

// retune moves the centre of a shared device (device.retune; the media
// endpoint checked the retune right). The listeners keep their frequency;
// a demodulator left outside the new band moves to the start frequency of
// the active preset when it is in the band, otherwise to the new centre.
func (ss *session) retune(req rxv1.Envelope) {
	p, err := decode[media.DeviceRetune](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	s := ss.s

	s.switching.Lock()
	defer s.switching.Unlock()

	if ok, retry := s.retunes.Allow(p.DeviceID, s.now()); !ok {
		ss.peer.RateLimited(req, retry)

		return
	}

	listeners := s.listeners(p.DeviceID)
	old := centre(p.DeviceID, listeners)

	snap, err := s.devices.Retune(p.DeviceID, p.CenterHz, 0)
	if err != nil {
		ss.fail(req, err)

		return
	}

	start, half := snap.CenterHz, int64(snap.RateHz)/2
	if _, preset, _ := s.presets(p.DeviceID, snap.ActivePreset); preset != nil && abs(preset.StartFreq-snap.CenterHz) <= half {
		start = preset.StartFreq
	}

	// The device no longer runs its preset as switched: every listener gets
	// a device.config without it.
	cleared := snap.ActivePreset != ""
	if cleared {
		s.clearPreset(p.DeviceID)
	}

	for _, l := range listeners {
		l.centreMoved(p.DeviceID, old, start, nil, cleared)
	}

	ss.peer.Ack(req, media.DeviceRetune{CenterHz: snap.CenterHz})
}

// centre returns the centre frequency of a device seen by its listeners
// (0 when none is attached: no demodulator to move).
func centre(device string, listeners []*session) int64 {
	for _, l := range listeners {
		if a := l.attachedTo(device); a != nil {
			return a.lease.Snapshot().CenterHz
		}
	}

	return 0
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

// attachedTo returns the attachment of a device (nil: not attached).
func (ss *session) attachedTo(device string) *attached {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ss.closed {
		return nil
	}

	return ss.devices[device]
}

// demodsOn returns the demodulators of the session on a device.
func (ss *session) demodsOn(device string) []*demodState {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var out []*demodState

	for _, d := range ss.demods {
		if d.device == device {
			out = append(out, d)
		}
	}

	return out
}

// selectPreset switches the shared preset of a device (preset.select,
// §6.4): the device is retuned to the preset's centre and sample rate for
// every listener, the caller's demodulators take the preset's start mode,
// frequency and squelch, and every listener gets the new device.config.
// The token scope (preset) was checked by the media endpoint.
//
// Retune gate: without the retune right, the switch is refused when it
// would leave another listener's demodulator outside the new capture band.
// The other demodulators keep their frequency; with the retune right, one
// that falls outside the band moves to the preset's start frequency.
func (ss *session) selectPreset(req rxv1.Envelope) {
	p, err := decode[media.PresetSelect](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	preset, err := ss.s.resolve(p.DeviceID, p.PresetID)
	if err != nil {
		ss.fail(req, err)

		return
	}

	s := ss.s

	s.switching.Lock()
	defer s.switching.Unlock()

	a := ss.attachedTo(p.DeviceID)
	if a == nil {
		ss.peer.Fail(req, rxv1.CodeConflict, "attach the device first")

		return
	}

	before := a.lease.Snapshot()
	listeners := s.listeners(p.DeviceID)

	if !ss.peer.Claims().Allows(p.DeviceID, token.PermRetune) && outside(listeners, ss, p.DeviceID, before.CenterHz, preset) {
		ss.peer.Fail(req, rxv1.CodeForbidden,
			"another listener's demodulator would be outside the new capture band: the retune permission is required")

		return
	}

	if wait := s.lastSwitch[p.DeviceID].Add(PresetSwitchEvery).Sub(s.now()); wait > 0 {
		ss.peer.RateLimited(req, wait)

		return
	}

	snap, err := s.devices.Retune(p.DeviceID, preset.CenterFreq, preset.SampRate)
	if err != nil {
		ss.fail(req, err)

		return
	}

	if err := s.devices.SetActivePreset(p.DeviceID, p.PresetID); err != nil {
		ss.fail(req, err)

		return
	}

	snap.ActivePreset = p.PresetID
	s.lastSwitch[p.DeviceID] = s.now()
	s.applied[p.DeviceID] = activePreset{id: p.PresetID, p: preset}

	s.log.Info("preset selected", slog.String("device_id", p.DeviceID), slog.String("preset_id", p.PresetID),
		slog.Int64("center_hz", snap.CenterHz), slog.Int("sample_rate", snap.RateHz))

	for _, l := range listeners {
		var mine *ctl.Preset
		if l == ss {
			mine = &preset
		}

		l.centreMoved(p.DeviceID, before.CenterHz, preset.StartFreq, mine, true)
	}

	ss.peer.Ack(req, media.PresetSelected{ActivePresetID: p.PresetID})
}

// outside reports whether a demodulator of another listener than caller,
// tuned against centre, would fall outside the capture band of preset.
func outside(listeners []*session, caller *session, device string, center int64, preset ctl.Preset) bool {
	half := preset.SampRate / 2

	for _, l := range listeners {
		if l == caller {
			continue
		}

		for _, d := range l.demodsOn(device) {
			if f := center + d.demod.Params().OffsetHz - preset.CenterFreq; f < -half || f > half {
				return true
			}
		}
	}

	return false
}

// centreMoved moves the session's demodulators of a device after a centre
// change from oldCenter: they keep their frequency, and one left outside
// the new band moves to startHz. mine is the switched preset when this
// session selected it: its demodulators take the preset's start mode,
// frequency, squelch and noise reduction. config sends the new
// device.config (a preset switch, or a retune that cleared the active
// preset; any other retune sends device.config.patch only).
func (ss *session) centreMoved(device string, oldCenter, startHz int64, mine *ctl.Preset, config bool) {
	a := ss.attachedTo(device)
	if a == nil {
		return
	}

	snap := a.lease.Snapshot()

	for _, d := range ss.demodsOn(device) {
		ss.moveDemod(d, oldCenter, startHz, snap, mine)
	}

	if !config {
		return
	}

	ss.mu.Lock()
	a.revision++
	rev := a.revision
	ss.mu.Unlock()

	ss.peer.Send(rxv1.TypeDeviceConfig, ss.deviceConfig(rev, snap, a.lease.Engine().Spectrum()))
}

// moveDemod moves one demodulator after a centre change (see
// centreMoved), atomically with the listener's own changes. When the
// engine refuses the preset's mode, squelch or noise reduction, only the
// offset moves.
func (ss *session) moveDemod(d *demodState, oldCenter, startHz int64, snap domain.Snapshot, mine *ctl.Preset) {
	d.mu.Lock()
	defer d.mu.Unlock()

	half := int64(snap.RateHz) / 2
	start := startHz - snap.CenterHz
	cur := d.demod.Params()
	moved := cur

	if mine != nil {
		moved.OffsetHz = start
	} else if moved.OffsetHz = oldCenter + cur.OffsetHz - snap.CenterHz; abs(moved.OffsetHz) > half {
		moved.OffsetHz = start
	}

	params := moved

	if mine != nil {
		// A new mode takes its default pass band (demod.set).
		if mine.StartMod != cur.Mode {
			params.Mode, params.LowHz, params.HighHz = mine.StartMod, 0, 0
		}

		if q := mine.InitialSquelchLevel; q != nil {
			params.SquelchDB = new(float64(*q))
		}

		if n := mine.InitialNRLevel; n != nil {
			params.NR = app.NR{Enabled: true, ThresholdDB: float64(*n)}
		}
	}

	if params == cur {
		return
	}

	err := d.demod.Set(params)
	if err != nil && params != moved {
		err = d.demod.Set(moved)
	}

	if err != nil {
		ss.s.log.Warn("demodulator not moved after a centre change", slog.String("device_id", d.device),
			slog.String("demod_id", d.id), slog.Any("error", err))

		return
	}

	res := applied(d.demod.Params())
	ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: d.stream, Applied: &res})
}

// Reauthorize implements media.StreamSession: after auth.refresh, devices
// the new token no longer lets the connection listen to are detached, and
// demodulators it no longer allows (scope, or beyond lim.max_demods,
// newest first) are removed.
func (ss *session) Reauthorize() {
	c := ss.peer.Claims()

	maxDemods := c.Limits.MaxDemods
	if maxDemods <= 0 {
		maxDemods = defaultMaxDemods
	}

	ss.mu.Lock()

	var (
		lost    []*attached
		removed []*demodState
	)

	for id, a := range ss.devices {
		if !c.Allows(id, token.PermListen) {
			lost = append(lost, a)
			delete(ss.devices, id)
		}
	}

	ids := slices.Sorted(maps.Keys(ss.demods))
	slices.SortFunc(ids, func(a, b string) int { return demodSeq(a) - demodSeq(b) })

	kept := 0

	for _, id := range ids {
		d := ss.demods[id]
		_, attachedOK := ss.devices[d.device]

		if !attachedOK || !c.Allows(d.device, token.PermDemod) || kept >= maxDemods {
			removed = append(removed, d)
			delete(ss.demods, id)

			continue
		}

		kept++
	}
	ss.mu.Unlock()

	for _, d := range removed {
		ss.closeDemod(d)
	}

	for _, a := range lost {
		ss.release(a)
	}

	ss.report()
}

// report tells the peer which device the connection listens to, for the hub
// presence (listener counts): the device of its first demodulator, else
// its first attached device, with that demodulator's mode. Only changes
// are reported.
func (ss *session) report() {
	ss.mu.Lock()

	device, mode := "", ""

	if ids := slices.Sorted(maps.Keys(ss.demods)); len(ids) > 0 {
		slices.SortFunc(ids, func(a, b string) int { return demodSeq(a) - demodSeq(b) })
		d := ss.demods[ids[0]]
		device, mode = d.device, d.demod.Params().Mode
	} else if ids := slices.Sorted(maps.Keys(ss.devices)); len(ids) > 0 {
		device = ids[0]
	}

	changed := device != ss.reported[0] || mode != ss.reported[1]
	ss.reported = [2]string{device, mode}
	ss.mu.Unlock()

	if changed {
		ss.peer.Attached(device, mode)
	}
}

// demodSeq is the creation order of a demodulator id ("d<n>").
func demodSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "d"))

	return n
}

// Close implements media.StreamSession.
func (ss *session) Close() {
	ss.s.mu.Lock()
	delete(ss.s.sessions, ss)
	ss.s.mu.Unlock()

	ss.mu.Lock()
	ss.closed = true
	devices := ss.devices
	demods := ss.demods
	ss.devices, ss.demods = map[string]*attached{}, map[string]*demodState{}
	ss.mu.Unlock()

	for _, d := range demods {
		d.demod.Close()
	}

	for _, a := range devices {
		if a.unsub != nil {
			a.unsub()
		}

		if a.unwatch != nil {
			a.unwatch()
		}

		a.lease.Release()
	}
}
