package http

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

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
	case rxv1.TypeDecoderSet:
		ss.setDecoder(req)
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
		Decoders: ss.digitalModes(),
	}

	if p := active; p != nil {
		cfg.Start = media.Start{Mode: p.StartMod}

		// A retune since the switch may leave the start outside the band.
		if off := p.StartFreq - snap.CenterHz; snap.Tuning().ContainsOffset(off) {
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

// badFFTCodec refuses req when codec ("" for the default) is not the FFT
// codec of this node; it reports whether it did.
func (ss *session) badFFTCodec(req rxv1.Envelope, codec string) bool {
	if codec == "" || codec == media.CodecFFTU8 {
		return false
	}

	ss.peer.Fail(req, rxv1.CodeOutOfRange, "fft codec "+strconv.Quote(codec)+" is not provided by this node (u8-db)")

	return true
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

	if ss.badFFTCodec(req, p.FFT.Codec) {
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

	ss.release(a, true)
	ss.peer.Ack(req, struct{}{})
	ss.report()
}

// release ends the attachment of a device and gives its lease back;
// notify closes its FFT stream on the client.
func (ss *session) release(a *attached, notify bool) {
	if a.unsub != nil {
		a.unsub()
	}

	if a.unwatch != nil {
		a.unwatch()
	}

	if notify {
		ss.q.RemoveStream(a.stream)
		ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: a.stream, Reason: media.ReasonClosed})
	}

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

		if ss.configureSecondary(req, p) {
			return
		}

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

	if p.Codec != nil && ss.badFFTCodec(req, *p.Codec) {
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

// Reauthorize implements media.StreamSession: after auth.refresh, devices
// the new token no longer lets the connection listen to are detached, and
// demodulators it no longer allows (scope, or beyond lim.max_demods,
// newest first) are removed.
func (ss *session) Reauthorize() {
	c := ss.peer.Claims()
	maxDemods := ss.maxDemods()

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

	kept := 0

	for _, id := range ss.demodIDs() {
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
		ss.release(a, true)
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

	if ids := ss.demodIDs(); len(ids) > 0 {
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

// attachedTo returns the attachment of a device (nil: not attached).
func (ss *session) attachedTo(device string) *attached {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ss.closed {
		return nil
	}

	return ss.devices[device]
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
		d.mu.Lock()
		ss.stopDecoder(d, false)
		d.mu.Unlock()

		d.demod.Close()
	}

	for _, a := range devices {
		ss.release(a, false)
	}
}
