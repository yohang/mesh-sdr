package http

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
)

type demodState struct {
	id     string
	device string
	stream uint16
	demod  app.Demod
	// mu makes each read-modify-write of the parameters atomic: the
	// listener's demod.set, decoder.set and audio.configure, and the moves
	// of a shared centre change made by another session. It guards dec.
	mu sync.Mutex
	// dec is the decoder of the demodulator (DEC-002), nil when none.
	dec *decoderSession
}

func wireCodec(c app.AudioCodec) rxv1.Codec {
	if c == app.CodecPCM {
		return rxv1.CodecPCMS16LE
	}

	return rxv1.CodecADPCMIMA
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

	maxDemods := ss.maxDemods()

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

	if !a.lease.Snapshot().Tuning().ContainsOffset(p.OffsetHz) {
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
		if b := meterJSON(id, m, ss.s.now()); b != nil {
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

func meterJSON(id string, m app.Meter, now time.Time) []byte {
	env, err := rxv1.NewEnvelope(rxv1.TypeDemodMeter, rxv1.CorrelationID{}, now.UnixMilli(), media.DemodMeter{DemodID: id, LevelDB: m.LevelDB, SquelchOpen: m.Open})
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

		if a == nil || !a.lease.Snapshot().Tuning().ContainsOffset(*p.OffsetHz) {
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

	before := d.demod.Params()

	if err := d.demod.Set(params); err != nil {
		ss.fail(req, err)

		return
	}

	switch after := d.demod.Params(); {
	case after.Mode != before.Mode:
		ss.underlyingChanged(d)
	case after.OffsetHz != before.OffsetHz:
		dialChanged(d)
	}

	ss.peer.Ack(req, media.AppliedResult{Applied: appliedFor(d)})

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
	d.mu.Lock()
	ss.stopDecoder(d, true)
	d.mu.Unlock()

	d.demod.Close()
	ss.q.RemoveStream(d.stream)
	ss.peer.Send(rxv1.TypeStreamClose, media.StreamClose{StreamID: d.stream, Reason: media.ReasonClosed})
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

// demodIDs returns the ids of the demodulators in creation order ("d<n>",
// ss.mu held).
func (ss *session) demodIDs() []string {
	ids := slices.Collect(maps.Keys(ss.demods))
	slices.SortFunc(ids, func(a, b string) int { return demodSeq(a) - demodSeq(b) })

	return ids
}

// demodSeq is the creation order of a demodulator id ("d<n>").
func demodSeq(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "d"))

	return n
}

// maxDemods is the demodulator limit of the connection (lim.max_demods).
func (ss *session) maxDemods() int {
	if n := ss.peer.Claims().Limits.MaxDemods; n > 0 {
		return n
	}

	return defaultMaxDemods
}
