package http

import (
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

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

	start := snap.CenterHz
	if _, preset, _ := s.presets(p.DeviceID, snap.ActivePreset); preset != nil && snap.Tuning().ContainsOffset(preset.StartFreq-snap.CenterHz) {
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

	start := startHz - snap.CenterHz
	cur := d.demod.Params()
	moved := cur

	if mine != nil {
		moved.OffsetHz = start
	} else if moved.OffsetHz = oldCenter + cur.OffsetHz - snap.CenterHz; !snap.Tuning().ContainsOffset(moved.OffsetHz) {
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

	switch after := d.demod.Params(); {
	case after.Mode != cur.Mode:
		ss.underlyingChanged(d)
	case snap.CenterHz+after.OffsetHz != oldCenter+cur.OffsetHz:
		dialChanged(d)
	}

	res := appliedFor(d)
	ss.peer.Send(rxv1.TypeStreamUpdate, media.StreamUpdate{StreamID: d.stream, Applied: &res})
}
