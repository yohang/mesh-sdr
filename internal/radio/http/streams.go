// Package http serves the device messages of the node media WebSocket
// (TECHNICAL_SPEC §6.4, §6.5): device.attach / detach, stream.configure,
// audio.configure, demod.create / set / remove and device.retune. The
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Device defaults sent in device.config until the hub pushes settings
// (FEATURE_SPEC defaults of waterfall.*, dsp.squelch_auto_margin; ADR 0019).
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
	Retune(id string, hz int64) (domain.Snapshot, error)
	Watch(id string, fn func(domain.Snapshot)) (func(), error)
}

// Streams implements media.Streams.
type Streams struct {
	devices Devices
	log     *slog.Logger
}

// NewStreams returns the handler.
func NewStreams(d Devices, log *slog.Logger) *Streams {
	return &Streams{devices: d, log: log}
}

// Open implements media.Streams.
func (s *Streams) Open(p media.Peer) media.StreamSession {
	return &session{
		s: s, peer: p, q: p.Queue(), next: 1,
		devices: map[string]*attached{}, demods: map[string]*demodState{},
		audio: audioConfig{codec: preferredCodec(p.Hello()), rate: dsp.DefaultOutputRate},
	}
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
	state    domain.State
}

type demodState struct {
	id     string
	device string
	stream uint16
	demod  app.Demod
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

func (ss *session) deviceConfig(a *attached, snap domain.Snapshot, info app.SpectrumInfo) media.DeviceConfig {
	c := ss.peer.Claims()
	perm := c.Allows(snap.ID, token.PermRetune)

	return media.DeviceConfig{
		DeviceID: snap.ID, Revision: a.revision, CenterHz: snap.CenterHz, SampleRate: snap.RateHz,
		PresetsAvailable: []media.PresetRef{}, TuningStepHz: tuningStepHz,
		Start:     media.Start{Mode: media.ModeNFM},
		Waterfall: media.Waterfall{Levels: media.Levels{Min: waterfallMin, Max: waterfallMax}, AutoMinRange: autoMinRange, Scheme: waterfallScheme},
		FFT:       media.FFTConfig{Size: info.Size, FPS: info.FPS},
		Squelch:   media.Squelch{Initial: squelchInitial, AutoMargin: squelchMargin},
		Limits:    media.FreqLimits{MinHz: snap.MinHz, MaxHz: snap.MaxHz},
		Permissions: media.Permissions{
			Preset: c.Allows(snap.ID, token.PermPreset), Retune: perm,
		},
	}
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
	a := &attached{id: p.DeviceID, lease: lease, stream: ss.streamID(), fps: fps, center: snap.CenterHz, state: snap.State}
	ss.devices[p.DeviceID] = a
	ss.mu.Unlock()

	open := media.StreamOpen{StreamID: a.stream, Kind: media.KindFFT, Codec: media.CodecFFTU8, FFT: fftOf(info), FPS: fps}
	cfg := ss.deviceConfig(a, snap, info)

	ss.peer.Ack(req, media.AttachResult{Device: cfg, Streams: []media.StreamOpen{open}})
	ss.peer.Send(rxv1.TypeDeviceConfig, cfg)
	ss.peer.Send(rxv1.TypeDeviceState, media.DeviceState{DeviceID: snap.ID, State: string(snap.State), Reason: snap.Reason})
	ss.peer.Send(rxv1.TypeStreamOpen, open)

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
	retuned := s.CenterHz != a.center
	a.state, a.center = s.State, s.CenterHz

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
			DeviceID: s.ID, Revision: rev, Set: map[string]any{"center_hz": s.CenterHz}, Unset: []string{},
		})
		ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: a.stream, FFT: fftOf(a.lease.Engine().Spectrum())})
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
		old := d.demod.Params()
		params := old
		params.Codec, params.OutputRate = codec, p.SampleRate

		if err := d.demod.Set(params); err != nil {
			for _, c := range done {
				_ = c.d.demod.Set(c.old)
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
}

func (ss *session) closeDemod(d *demodState) {
	d.demod.Close()
	ss.q.RemoveStream(d.stream)
	ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: d.stream, Reason: media.ReasonClosed})
}

func (ss *session) retune(req rxv1.Envelope) {
	p, err := decode[media.DeviceRetune](req)
	if err != nil {
		ss.fail(req, err)

		return
	}

	snap, err := ss.s.devices.Retune(p.DeviceID, p.CenterHz)
	if err != nil {
		ss.fail(req, err)

		return
	}

	ss.peer.Ack(req, media.DeviceRetune{CenterHz: snap.CenterHz})
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
}

// demodSeq is the creation order of a demodulator id ("d<n>").
func demodSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "d"))

	return n
}

// Close implements media.StreamSession.
func (ss *session) Close() {
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
