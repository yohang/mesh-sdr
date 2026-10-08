package http

import (
	"errors"
	"log/slog"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
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
}

// unavailableText is the reason non-admins see for a digital mode the node
// cannot run (DIAG-004: admins see the missing tool).
const unavailableText = "not available on this receiver"

// adminRole is the admin role name in access tokens.
const adminRole = "admin"

// decoderSession is the decoder running on a demodulator (one at most).
type decoderSession struct {
	id    shared.UUID
	mode  domain.DigitalMode
	run   app.DecoderRun
	untap func()
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
		dm := media.DigitalMode{Mode: st.Mode.Name, Label: st.Mode.Label, Underlying: st.Mode.Underlying, Available: st.Available}

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

// setDecoder handles decoder.set (§6.4, DEC-002): the owner of a
// demodulator starts or stops its decoder. The mode is checked against the
// catalogue, the node capabilities and the service-only flag; a
// demodulator whose mode the digital mode does not allow switches to its
// default underlying mode first (DEC-003).
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
		ss.peer.Ack(req, media.DecoderStarted{Applied: appliedFor(d)})

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

	params := d.demod.Params()
	switched := !m.Allows(params.Mode)

	if switched {
		params.Mode, params.LowHz, params.HighHz = m.DefaultUnderlying(), 0, 0

		if err := d.demod.Set(params); err != nil {
			ss.fail(req, err)

			return
		}
	}

	ss.stopDecoder(d, false)

	if err := ss.startDecoder(d, m); err != nil {
		ss.decoderError(req, err)

		return
	}

	ss.peer.Ack(req, media.DecoderStarted{DecoderSessionID: d.dec.id.String(), Applied: appliedFor(d)})

	if switched {
		ss.report()
	}
}

// underlyingChanged applies a change of the demodulator's mode to its
// decoder (DEC-003): an allowed mode re-creates the decoder, another one
// stops it (d.mu held).
func (ss *session) underlyingChanged(d *demodState) {
	if d.dec == nil {
		return
	}

	m := d.dec.mode
	ss.stopDecoder(d, true)

	if !m.Allows(d.demod.Params().Mode) {
		return
	}

	if err := ss.startDecoder(d, m); err != nil {
		ss.s.log.Warn("decoder not re-created after an underlying mode change", slog.String("demod_id", d.id),
			slog.String("decoder", m.Name), slog.Any("error", err))
	}
}

// startDecoder starts a session of m on d and taps its audio (d.mu held).
func (ss *session) startDecoder(d *demodState, m domain.DigitalMode) error {
	id, run, err := ss.s.dec.Decoders.Start(m, func(id shared.UUID) app.DecoderEvents {
		return app.DecoderEvents{
			Decode: func(rec app.DecodeRecord) { ss.decoded(d, id, m, rec) },
			Status: func(st app.DecoderStatus) { ss.decoderStatus(d, id, m.Name, st.State, st.Reason) },
		}
	})
	if err != nil {
		return err
	}

	d.dec = &decoderSession{id: id, mode: m, run: run, untap: d.demod.Tap(run.Audio)}

	return nil
}

// stopDecoder ends the decoder of d, if any; notify sends its stopped
// status (d.mu held).
func (ss *session) stopDecoder(d *demodState, notify bool) {
	dec := d.dec
	if dec == nil {
		return
	}

	d.dec = nil
	dec.untap()
	dec.run.Close()

	if notify {
		ss.decoderStatus(d, dec.id, dec.mode.Name, media.DecoderStopped, "")
	}
}

func (ss *session) decoderStatus(d *demodState, id shared.UUID, mode, state, reason string) {
	ss.peer.Send(rxv1.TypeDiagState, media.DiagState{
		DemodID: d.id, DecoderSessionID: id.String(), Decoder: mode, State: state, Reason: reason, Since: time.Now().UnixMilli(),
	})
}

// decoded sends a decoded message to the listener (decode) and to the hub
// (decode.batch). Its frequency is the demodulator's dial frequency.
func (ss *session) decoded(d *demodState, id shared.UUID, m domain.DigitalMode, rec app.DecodeRecord) {
	a := ss.attachedTo(d.device)
	if a == nil {
		return
	}

	snap := a.lease.Snapshot()
	freq := snap.CenterHz + d.demod.Params().OffsetHz
	text := capText(rec.Text, media.MaxDecodeText)

	ss.peer.Send(rxv1.TypeDecode, media.Decode{
		DemodID: d.id, DecoderSessionID: id.String(), Mode: m.Name, TS: rec.Time.UnixMilli(), FreqHz: freq,
		Schema: rec.Schema, Text: text, Payload: rec.Payload,
	})

	if pub := ss.s.dec.Publisher; pub != nil {
		pub.Decoded(app.Decoded{
			DeviceID: d.device, SessionID: id.String(), PresetID: snap.ActivePreset, Mode: m.Name, Family: m.Family,
			FreqHz: freq, Time: rec.Time, CID: ss.peer.Claims().ConnectionID, Schema: rec.Schema, Text: text, Payload: rec.Payload,
		})
	}
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
