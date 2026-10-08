package http

import (
	"errors"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Decoding are the digital decoders of the stream handler (DEC-002): the
// decoder sessions of listeners and where their messages go besides the
// listener (the hub, DEC-047). A zero Decoding offers no digital mode.
type Decoding struct {
	Decoders  *app.Decoders
	Publisher app.DecodePublisher
	// Files receives the images of the decoders (FIL-005).
	Files app.FilePublisher
}

// decoder.set limit per connection: a decoder start spawns a process.
const (
	DecoderSetEvery = time.Second
	DecoderSetBurst = 3
)

// unavailableText is the reason non-admins see for a digital mode the node
// cannot run (DIAG-004: admins see the missing tool).
const unavailableText = "not available on this receiver"

// nodeBusy is the reason of a decoder refused beyond decoders.max_sessions.
const nodeBusy = "node busy"

// adminRole is the admin role name in access tokens.
const adminRole = "admin"

// decoderSession is the decoder running on a demodulator (one at most).
type decoderSession struct {
	id      shared.UUID
	mode    domain.DigitalMode
	variant string
	run     app.DecoderRun
	untap   func()
	// offset is the secondary offset of a text decoder (DEC-005).
	offset int64
	// fft is the secondary FFT stream (DEC-004), when hasFFT; it starts
	// paused: the listener opens it while it shows the Decoders tab.
	fft    uint16
	hasFFT bool
	fps    int
	paused bool
}

// digitalModes lists the catalogue for device.config.
func (ss *session) digitalModes() []media.DigitalMode {
	out := []media.DigitalMode{}

	dec := ss.s.dec.Decoders
	if dec == nil {
		return out
	}

	admin := slices.Contains(ss.peer.Claims().Roles, adminRole)

	for _, st := range dec.Catalogue() {
		dm := media.DigitalMode{
			Mode: st.Mode.Name, Label: st.Mode.Label, Underlying: st.Mode.Underlying, Variants: st.Mode.Variants, Available: st.Available,
		}

		if dm.Variants == nil {
			dm.Variants = []string{}
		}

		if !st.Available {
			dm.Reason = unavailableText
			if admin && st.Reason != "" {
				dm.Reason = st.Reason
			}
		}

		out = append(out, dm)
	}

	return out
}

// appliedFor returns the parameters of a demodulator with its decoder
// (d.mu held).
func appliedFor(d *demodState) media.Applied {
	a := applied(d.demod.Params())

	if d.dec != nil {
		name := d.dec.mode.Name
		a.Decoder = &name
	}

	return a
}

// decoderError maps a decoder refusal to its protocol error.
func (ss *session) decoderError(req rxv1.Envelope, err error) {
	var de *shared.Error

	switch {
	case errors.Is(err, domain.ErrUnknownDecoder):
		ss.peer.Fail(req, rxv1.CodeNotFound, "unknown digital mode")
	case errors.Is(err, domain.ErrDecoderServiceOnly):
		ss.peer.Fail(req, rxv1.CodeForbidden, "this digital mode runs only as a background service")
	case errors.Is(err, domain.ErrDecoderUnavailable) && errors.As(err, &de):
		reason := de.Message()
		if !slices.Contains(ss.peer.Claims().Roles, adminRole) {
			reason = "this digital mode is " + unavailableText
		}

		ss.peer.Fail(req, rxv1.CodeDemodError, reason)
	default:
		ss.s.log.Error("decoder not started", slog.Any("error", err))
		ss.peer.Fail(req, rxv1.CodeDemodError, "the decoder could not be started")
	}
}

// variantOf reads options.variant of decoder.set.
func variantOf(p media.DecoderSet) (string, bool) {
	v, ok := p.Options["variant"]
	if !ok || v == nil {
		return "", true
	}

	s, ok := v.(string)

	return s, ok
}

// offsetOf returns the secondary offset of a text decoder: the one asked
// for, else the running decoder's, else the middle of the demodulator's
// pass band (DEC-005). Other modes have none.
func offsetOf(m domain.DigitalMode, asked *int64, d *demodState, params app.DemodParams) int64 {
	switch {
	case m.BandwidthHz <= 0:
		return 0
	case asked != nil:
		return *asked
	case d.dec != nil && d.dec.mode.BandwidthHz > 0:
		return d.dec.offset
	default:
		return int64(math.Round((params.LowHz + params.HighHz) / 2))
	}
}

// startedFor is the ack result of decoder.set for the decoder of d (d.mu
// held).
func startedFor(d *demodState) media.DecoderStarted {
	res := media.DecoderStarted{Applied: appliedFor(d)}

	if dec := d.dec; dec != nil {
		res.DecoderSessionID, res.Variant = dec.id.String(), dec.variant

		if dec.mode.BandwidthHz > 0 {
			res.OffsetHz, res.BandwidthHz = new(dec.offset), dec.mode.BandwidthHz
		}

		if dec.hasFFT {
			res.SecondaryFFTStreamID = new(dec.fft)
		}
	}

	return res
}

// setDecoder handles decoder.set (§6.4, DEC-002): the owner of a
// demodulator starts or stops its decoder. The mode is checked against the
// catalogue, the node capabilities and the service-only flag; a
// demodulator whose mode the digital mode does not allow switches to its
// default underlying mode first (DEC-003). Beyond decoders.max_sessions the
// decoder is unavailable (node busy). An offset_hz for the text decoder
// already running moves its secondary selector (DEC-005); otherwise the
// decoder (re)starts, at most DecoderSetBurst times per DecoderSetEvery.
func (ss *session) setDecoder(req rxv1.Envelope) {
	p, err := decode[media.DecoderSet](req)
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

	if p.Decoder == nil || *p.Decoder == "" {
		ss.stopDecoder(d, true)
		ss.peer.Ack(req, startedFor(d))

		return
	}

	if ss.retuneDecoder(req, d, p) {
		return
	}

	if ok, retry := ss.s.decoderSets.Allow(ss.peer.Claims().ConnectionID, ss.s.now()); !ok {
		ss.peer.RateLimited(req, retry)

		return
	}

	dec := ss.s.dec.Decoders
	if dec == nil {
		ss.decoderError(req, domain.ErrUnknownDecoder)

		return
	}

	m, err := dec.Check(*p.Decoder)
	if err != nil {
		ss.decoderError(req, err)

		return
	}

	asked, ok := variantOf(p)
	variant, known := m.Variant(asked)

	if !ok || !known {
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "options.variant: not a variant of "+m.Label)

		return
	}

	params := d.demod.Params()
	switched := !m.Allows(params.Mode)

	if switched {
		params.Mode, params.LowHz, params.HighHz = m.DefaultUnderlying(), 0, 0
	}

	// A mode with a pass band sets it (WSJT 0 to 3000 Hz, WSPR 1350 to
	// 1650 Hz, DEC-020).
	if (m.LowHz != 0 || m.HighHz != 0) && (params.LowHz != m.LowHz || params.HighHz != m.HighHz) {
		params.LowHz, params.HighHz = m.LowHz, m.HighHz
		switched = true
	}

	if switched {
		if err := d.demod.Set(params); err != nil {
			ss.fail(req, err)

			return
		}
	}

	// A new mode has its default pass band: the default offset follows.
	offset := offsetOf(m, p.OffsetHz, d, d.demod.Params())
	if err := m.CheckOffset(float64(offset)); err != nil {
		if switched {
			res := appliedFor(d)
			ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: d.stream, Applied: &res})
			ss.report()
		}

		ss.fail(req, err)

		return
	}

	ss.stopDecoder(d, true)

	err = ss.startDecoder(d, m, variant, offset)

	switch {
	case errors.Is(err, app.ErrNodeBusy):
		ss.peer.Ack(req, startedFor(d))
		ss.decoderStatus(d, shared.UUID{}, m.Name, variant, media.DecoderUnavailable, nodeBusy)
	case errors.As(err, new(errWideTap)):
		ss.peer.Ack(req, startedFor(d))
		ss.decoderStatus(d, shared.UUID{}, m.Name, variant, media.DecoderUnavailable, wideReason(err))
	case err != nil:
		// The listener learns the underlying mode it was switched to.
		if switched {
			res := appliedFor(d)
			ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: d.stream, Applied: &res})
		}

		ss.decoderError(req, err)
	default:
		ss.peer.Ack(req, startedFor(d))
	}

	if switched {
		ss.report()
	}
}

// retuneDecoder moves the secondary selector of the running text decoder
// when decoder.set names it again (same variant) with an offset_hz
// (DEC-005): no new session. It reports whether it answered req (d.mu
// held).
func (ss *session) retuneDecoder(req rxv1.Envelope, d *demodState, p media.DecoderSet) bool {
	dec := d.dec
	if dec == nil || p.OffsetHz == nil || dec.mode.BandwidthHz <= 0 || dec.mode.Name != *p.Decoder {
		return false
	}

	if v, ok := variantOf(p); !ok || v != "" && v != dec.variant {
		return false
	}

	if err := dec.mode.CheckOffset(float64(*p.OffsetHz)); err != nil {
		ss.fail(req, err)

		return true
	}

	dec.offset = *p.OffsetHz
	dec.run.Retune(float64(dec.offset))
	ss.peer.Ack(req, startedFor(d))

	return true
}

// dialChanged resets what the decoder of d learnt of the dial frequency
// (the CW timing, DEC-012) after the demodulator moved (d.mu held).
func dialChanged(d *demodState) {
	if d.dec != nil {
		d.dec.run.Retune(float64(d.dec.offset))
	}
}

// underlyingChanged applies a change of the demodulator's mode to its
// decoder (DEC-003): an allowed mode re-creates the decoder, another one
// stops it (d.mu held).
func (ss *session) underlyingChanged(d *demodState) {
	if d.dec == nil {
		return
	}

	m, variant, offset := d.dec.mode, d.dec.variant, d.dec.offset
	ss.stopDecoder(d, true)

	params := d.demod.Params()
	if !m.Allows(params.Mode) {
		return
	}

	// An offset outside the new pass band (the sideband flipped) moves to
	// its middle.
	if m.BandwidthHz > 0 && (float64(offset) < params.LowHz || float64(offset) > params.HighHz) {
		offset = offsetOf(m, nil, &demodState{}, params)
	}

	err := ss.startDecoder(d, m, variant, offset)

	switch {
	case errors.Is(err, app.ErrNodeBusy):
		ss.decoderStatus(d, shared.UUID{}, m.Name, variant, media.DecoderUnavailable, nodeBusy)
	case errors.As(err, new(errWideTap)):
		ss.decoderStatus(d, shared.UUID{}, m.Name, variant, media.DecoderUnavailable, wideReason(err))
	case err != nil:
		ss.s.log.Warn("decoder not re-created after an underlying mode change", slog.String("demod_id", d.id),
			slog.String("decoder", m.Name), slog.Any("error", err))
		ss.decoderStatus(d, shared.UUID{}, m.Name, variant, media.DecoderError, "start_failed")
	}
}

// reception is where the last kept message of a decoder session was
// received: the files of the session are stamped with it (FIL-008: the
// image start is the last message kept before the image ends).
type reception struct {
	mu     sync.Mutex
	freq   int64
	preset string
}

func (r *reception) set(freq int64, preset string) {
	r.mu.Lock()
	r.freq, r.preset = freq, preset
	r.mu.Unlock()
}

func (r *reception) get() (int64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.freq, r.preset
}

// startDecoder starts a session of m on d and taps its audio, or the
// selector IQ of a text decoder; a mode with a secondary FFT gets its
// stream, paused (d.mu held).
func (ss *session) startDecoder(d *demodState, m domain.DigitalMode, variant string, offset int64) error {
	// fft is set before the session can send a line (Spectrum).
	var (
		fft      uint16
		wideRun  atomic.Pointer[app.DecoderRun]
		wideStop func()
		wideID   atomic.Pointer[shared.UUID]
	)

	// A wide IQ decoder gets its channel first: a device rate below the
	// mode's, or a band outside the device span, makes it unavailable.
	if m.Input == domain.InputWideIQ {
		cancel, err := d.demod.TapWideIQ(m.InputRate, m.BandLow, m.BandHigh, func(b app.WideIQBlock) {
			if r := wideRun.Load(); r != nil {
				(*r).WideIQ(b)
			}
		}, func(reason string) {
			if id := wideID.Load(); id != nil {
				ss.sessionStatus(d, *id, m.Name, variant, app.DecoderStatus{State: app.DecoderUnavailable, Reason: reason})
			}
		})
		if err != nil {
			return errWideTap{err}
		}

		wideStop = cancel
	}

	rx := &reception{}
	spec := app.DecoderSpec{Mode: m, Variant: variant, OffsetHz: float64(offset), DialHz: func() int64 { return ss.tuning(d).dial }}

	id, run, err := ss.s.dec.Decoders.Start(spec, func(id shared.UUID) app.DecoderEvents {
		return app.DecoderEvents{
			Decode:   func(rec app.DecodeRecord) { ss.decoded(d, id, m, rec, rx) },
			Status:   func(st app.DecoderStatus) { ss.sessionStatus(d, id, m.Name, variant, st) },
			File:     func(f app.ProducedFile) { ss.produced(d, id, m, rx, f) },
			Spectrum: func(f app.SpectrumFrame) { ss.secondaryFrame(d, id, fft, f) },
			Dial:     func() int64 { return ss.dial(d) },
		}
	})
	if err != nil {
		if wideStop != nil {
			wideStop()
		}

		return err
	}

	dec := &decoderSession{id: id, mode: m, variant: variant, run: run, offset: offset}

	switch m.Input {
	case domain.InputNarrowIQ:
		dec.untap = d.demod.TapIQ(run.IQ)
	case domain.InputWideIQ:
		wideID.Store(&id)
		wideRun.Store(&run)
		dec.untap = wideStop
	default:
		dec.untap = d.demod.Tap(run.Audio)
	}

	if size := run.SpectrumSize(); m.SecondaryFFT && size > 0 {
		ss.mu.Lock()
		fft = ss.streamID()
		ss.mu.Unlock()

		dec.fft, dec.hasFFT, dec.fps, dec.paused = fft, true, ss.secondaryFPS(d.device), true
		ss.q.ConfigureFFT(fft, dec.fps, true)
		ss.peer.Send(rxv1.TypeStreamOpen, media.StreamOpen{
			StreamID: fft, Kind: media.KindFFT2, Codec: media.CodecFFTU8, FFT: secondaryFFT(m, size), DemodID: d.id, FPS: dec.fps, Paused: true,
		})
	}

	d.dec = dec

	return nil
}

// secondaryFFT is the geometry of a secondary FFT stream: start_hz and
// span_hz are relative to the decoder's dial frequency (the selector IQ at
// the mode's input rate, DEC-004).
func secondaryFFT(m domain.DigitalMode, size int) *media.StreamFFT {
	scale := rxv1.DefaultFFTU8Scale()

	return &media.StreamFFT{Size: size, StartHz: -int64(m.InputRate / 2), SpanHz: m.InputRate, DBMin: scale.DBMin, DBStep: scale.DBStep}
}

// secondaryFPS is the frame rate of a secondary FFT: the device's spectrum
// rate (fft_fps), within the client's max_fft_fps.
func (ss *session) secondaryFPS(device string) int {
	fps := 1

	if a := ss.attachedTo(device); a != nil {
		fps = a.lease.Engine().Spectrum().FPS
	}

	if m := ss.peer.Hello().Capabilities.MaxFFTFPS; m > 0 {
		fps = min(fps, m)
	}

	return max(fps, 1)
}

// secondaryFrame queues a secondary FFT line of the current session while
// its stream is open: d.mu is held so that a stopped session never queues
// a frame after its stream was removed.
func (ss *session) secondaryFrame(d *demodState, id shared.UUID, stream uint16, f app.SpectrumFrame) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if dec := d.dec; dec == nil || dec.id != id || !dec.hasFFT || dec.fft != stream || dec.paused {
		return
	}

	_ = ss.q.PushFFT(sendq.Frame{
		Type: rxv1.FrameSecondaryFFT, Codec: rxv1.CodecFFTU8DB, StreamID: stream, TimestampUS: f.TimestampUS, Payload: f.Payload,
	})
}

// secondaryOf returns the demodulator whose decoder has the secondary FFT
// stream id, locked (nil: none).
func (ss *session) secondaryOf(id uint16) *demodState {
	ss.mu.Lock()
	demods := make([]*demodState, 0, len(ss.demods))

	for _, d := range ss.demods {
		demods = append(demods, d)
	}
	ss.mu.Unlock()

	for _, d := range demods {
		d.mu.Lock()

		if d.dec != nil && d.dec.hasFFT && d.dec.fft == id {
			return d
		}

		d.mu.Unlock()
	}

	return nil
}

// configureSecondary applies stream.configure to the secondary FFT stream
// of a decoder: the listener opens it while it shows the Decoders tab, and
// the node computes it only then (DEC-004). It reports whether the stream
// is one.
func (ss *session) configureSecondary(req rxv1.Envelope, p media.StreamConfigure) bool {
	d := ss.secondaryOf(p.StreamID)
	if d == nil {
		return false
	}

	defer d.mu.Unlock()

	dec := d.dec
	limit := ss.secondaryFPS(d.device)

	switch {
	case p.FPS != nil && (*p.FPS < 1 || *p.FPS > limit):
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "fps: want 1.."+strconv.Itoa(limit))

		return true
	case p.Codec != nil && *p.Codec != media.CodecFFTU8:
		ss.peer.Fail(req, rxv1.CodeOutOfRange, "fft codec "+strconv.Quote(*p.Codec)+" is not provided by this node")

		return true
	}

	if p.FPS != nil {
		dec.fps = *p.FPS
	}

	if p.Paused != nil {
		dec.paused = *p.Paused
	}

	ss.q.ConfigureFFT(dec.fft, dec.fps, dec.paused)

	if dec.paused {
		dec.run.Spectrum(0)
	} else {
		dec.run.Spectrum(dec.fps)
	}
	ss.peer.Ack(req, media.StreamResult{Stream: media.StreamOpen{
		StreamID: dec.fft, Kind: media.KindFFT2, Codec: media.CodecFFTU8, FFT: secondaryFFT(dec.mode, dec.run.SpectrumSize()),
		DemodID: d.id, FPS: dec.fps, Paused: dec.paused,
	}})

	return true
}

// stopDecoder ends the decoder of d, if any, and closes its secondary FFT
// stream; notify sends its stopped status (d.mu held). Nothing of that
// session reaches the listener after.
func (ss *session) stopDecoder(d *demodState, notify bool) {
	dec := d.dec
	if dec == nil {
		return
	}

	d.dec = nil
	dec.untap()
	dec.run.Close()

	if dec.hasFFT {
		ss.q.RemoveStream(dec.fft)

		if notify {
			ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: dec.fft, Reason: media.ReasonClosed})
		}
	}

	if notify {
		ss.decoderStatus(d, dec.id, dec.mode.Name, dec.variant, media.DecoderStopped, "")
	}
}

// live reports whether id is the current decoder session of d.
func live(d *demodState, id shared.UUID) bool {
	_, ok := liveOffset(d, id)

	return ok
}

// liveOffset returns the secondary offset of session id while it is the
// current decoder session of d.
func liveOffset(d *demodState, id shared.UUID) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.dec == nil || d.dec.id != id {
		return 0, false
	}

	return d.dec.offset, true
}

// sessionStatus forwards a status of session id while it is current.
func (ss *session) sessionStatus(d *demodState, id shared.UUID, mode, variant string, st app.DecoderStatus) {
	if live(d, id) {
		ss.sendStatus(d, id, mode, variant, st)
	}
}

func (ss *session) decoderStatus(d *demodState, id shared.UUID, mode, variant, state, reason string) {
	ss.sendStatus(d, id, mode, variant, app.DecoderStatus{State: state, Reason: reason})
}

func (ss *session) sendStatus(d *demodState, id shared.UUID, mode, variant string, st app.DecoderStatus) {
	p := media.DiagState{
		DemodID: d.id, Decoder: mode, Variant: variant, State: st.State, Reason: st.Reason, Warning: st.Warning, Since: ss.s.now().UnixMilli(),
	}
	if !id.IsZero() {
		p.DecoderSessionID = id.String()
	}

	ss.peer.Send(rxv1.TypeDiagState, p)
}

// dial returns the dial frequency of d (0 when its device is detached).
func (ss *session) dial(d *demodState) int64 {
	a := ss.attachedTo(d.device)
	if a == nil {
		return 0
	}

	return a.lease.Snapshot().CenterHz + d.demod.Params().OffsetHz
}

// decoded sends a decoded message of the current session to the listener
// (decode) and, unless live or partial, to the hub (decode.batch). Its
// frequency is the dial frequency it was received on (a slot's at its
// start, else the demodulator's), plus the secondary offset of a text
// decoder (DEC-005) or the audio frequency of the signal a slot decoder
// reports (WSJT, JS8).
func (ss *session) decoded(d *demodState, id shared.UUID, m domain.DigitalMode, rec app.DecodeRecord, rx *reception) {
	offset, ok := liveOffset(d, id)
	if !ok {
		return
	}

	a := ss.attachedTo(d.device)
	if a == nil {
		return
	}

	snap := a.lease.Snapshot()

	dial := rec.DialHz
	if dial == 0 {
		dial = snap.CenterHz + d.demod.Params().OffsetHz
	}

	// A text decoder adds its secondary offset, a slot decoder the audio
	// frequency of the signal,
	// a skimmer the offset of its signal.
	freq := dial + offset + rec.AudioHz
	text := capText(rec.Text, media.MaxDecodeText)

	ss.peer.Send(rxv1.TypeDecode, media.Decode{
		DemodID: d.id, DecoderSessionID: id.String(), Mode: m.Name, TS: rec.Time.UnixMilli(), FreqHz: freq,
		Schema: rec.Schema, Text: text, Payload: rec.Payload, Partial: rec.Partial,
	})

	// The rows of an image and the line a text decoder is printing go to
	// the listener only.
	if rec.Live || rec.Partial {
		return
	}

	rx.set(freq, snap.ActivePreset)

	if pub := ss.s.dec.Publisher; pub != nil {
		pub.Decoded(app.Decoded{
			DeviceID: d.device, SessionID: id.String(), PresetID: snap.ActivePreset, Mode: m.Name, Family: m.Family,
			FreqHz: freq, Time: rec.Time, CID: ss.peer.Claims().ConnectionID, Schema: rec.Schema, Text: text, Payload: rec.Payload,
		})
	}
}

// produced sends a file of a decoder session to the hub (FIL-005), even
// once the session is over (its image in progress). An image is stamped
// with the reception of the session's last kept message (an image without
// its start is dropped); a file that carries its frequency (a skimmer text
// log) keeps it, with the device's active preset.
func (ss *session) produced(d *demodState, id shared.UUID, m domain.DigitalMode, rx *reception, f app.ProducedFile) {
	freq, preset := rx.get()
	if f.FreqHz > 0 {
		freq, preset = f.FreqHz, ss.tuning(d).preset
	}

	pub := ss.s.dec.Files
	if pub == nil || freq <= 0 {
		return
	}

	f.DeviceID, f.PresetID, f.SessionID, f.Mode, f.FreqHz = d.device, preset, id.String(), m.Name, freq
	pub.Produced(f)
}

// decoderTuning is where a demodulator receives.
type decoderTuning struct {
	device, preset string
	dial           int64
}

// tuning returns the device, active preset and dial frequency of d (no
// device when d's device is no longer attached). It does not take d.mu.
func (ss *session) tuning(d *demodState) decoderTuning {
	a := ss.attachedTo(d.device)
	if a == nil {
		return decoderTuning{}
	}

	snap := a.lease.Snapshot()

	return decoderTuning{device: d.device, preset: snap.ActivePreset, dial: snap.CenterHz + d.demod.Params().OffsetHz}
}

// errWideTap refuses a wide IQ decoder whose channel cannot be built (the
// device sample rate is below its input rate, or its band leaves the
// device span): the decoder is unavailable.
type errWideTap struct{ err error }

func (e errWideTap) Error() string { return e.err.Error() }

func (e errWideTap) Unwrap() error { return e.err }

// wideReason is the reason of an errWideTap shown to the listener.
func wideReason(err error) string {
	var de *shared.Error
	if errors.As(err, &de) {
		return de.Message()
	}

	return "the device cannot serve this decoder"
}

// capText cuts s to at most n bytes on a rune boundary.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
