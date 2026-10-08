package http_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// tools reports cap:multimon-ng and cap:native-dsp as ok or missing.
type tools struct{ ok bool }

func (t tools) Available(c string) (bool, string) {
	if (c == domain.CapMultimonNG || c == domain.CapNativeDSP || c == domain.CapWSPRD || c == domain.CapRTL433) && t.ok {
		return true, ""
	}

	return false, "multimon-ng not found"
}

// fakeRun records whether it was closed, its offsets and its secondary
// FFT state.
type fakeRun struct {
	mu      sync.Mutex
	ev      app.DecoderEvents
	mode    string
	variant string
	closed  bool
	offsets []float64
	size    int
	fps     int
}

func (r *fakeRun) Audio(app.AudioBlock)   {}
func (r *fakeRun) IQ(app.IQBlock)         {}
func (r *fakeRun) WideIQ(app.WideIQBlock) {}
func (r *fakeRun) SpectrumSize() int      { return r.size }

func (r *fakeRun) Retune(hz float64) {
	r.mu.Lock()
	r.offsets = append(r.offsets, hz)
	r.mu.Unlock()
}

func (r *fakeRun) Spectrum(fps int) {
	r.mu.Lock()
	r.fps = fps
	r.mu.Unlock()
}

func (r *fakeRun) state() ([]float64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]float64(nil), r.offsets...), r.fps
}

func (r *fakeRun) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func (r *fakeRun) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closed
}

type runner struct {
	mu   sync.Mutex
	runs []*fakeRun
}

func (r *runner) Start(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	run := &fakeRun{ev: ev, mode: spec.Mode.Name, variant: spec.Variant, offsets: []float64{spec.OffsetHz}}
	if spec.Mode.Cap == domain.CapNativeDSP {
		run.size = 2048
	}

	r.mu.Lock()
	r.runs = append(r.runs, run)
	r.mu.Unlock()

	return run, nil
}

func (r *runner) last() *fakeRun {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.runs[len(r.runs)-1]
}

type publisher struct {
	mu  sync.Mutex
	got []app.Decoded
}

func (p *publisher) Decoded(d app.Decoded) {
	p.mu.Lock()
	p.got = append(p.got, d)
	p.mu.Unlock()
}

func (p *publisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.got)
}

// sentOf returns the payloads of the messages of a type sent to the peer.
func sentOf[T any](p *peer, typ rxv1.MessageType) []T {
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []T

	for i, s := range p.sent {
		if s == typ {
			if v, ok := p.payload[i].(T); ok {
				out = append(out, v)
			}
		}
	}

	return out
}

// clock advances by step on every reading (no rate limit in the way).
func clock(step time.Duration) func() time.Time {
	var (
		mu sync.Mutex
		t  = time.Unix(1_800_000_000, 0)
	)

	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		t = t.Add(step)

		return t
	}
}

// files records the files of the decoders.
type files struct {
	mu  sync.Mutex
	got []app.ProducedFile
}

func (f *files) Produced(p app.ProducedFile) {
	f.mu.Lock()
	f.got = append(f.got, p)
	f.mu.Unlock()
}

func (f *files) all() []app.ProducedFile {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]app.ProducedFile(nil), f.got...)
}

type decoderEnv struct {
	t  *testing.T
	p  *peer
	ss interface {
		Handle(context.Context, rxv1.Envelope)
	}
	run   *runner
	pub   *publisher
	files *files
	dec   *app.Decoders
}

func newDecoderEnv(t *testing.T, ok bool, maxSessions int, step time.Duration, roles ...string) *decoderEnv {
	t.Helper()

	m := runManager(t)
	e := &decoderEnv{t: t, run: &runner{}, pub: &publisher{}, files: &files{}}
	e.dec = app.NewDecoders(tools{ok: ok}, e.run, maxSessions, time.Now)
	dec := radiohttp.Decoding{Decoders: e.dec, Publisher: e.pub, Files: e.files}

	e.p = &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen, token.PermDemod)}
	e.p.claims.ConnectionID = "c1"
	e.p.claims.Roles = roles

	streams := radiohttp.NewStreams(m, nil, dec, slog.New(slog.DiscardHandler))
	streams.SetNow(clock(step))

	ss := streams.Open(e.p)
	t.Cleanup(ss.Close)
	e.ss = ss

	return e
}

func (e *decoderEnv) check(typ rxv1.MessageType, payload any, want rxv1.ErrorCode) reply {
	e.t.Helper()
	e.ss.Handle(context.Background(), env(e.t, typ, payload))

	got := e.p.last()
	if want == "" && got.typ != rxv1.TypeAck || want != "" && got.code != want {
		e.t.Fatalf("%s %v: got %+v, want %q", typ, payload, got, want)
	}

	return got
}

// TestDecoderSet covers decoder.set (DEC-002, DEC-003): the allow-list,
// variants, the underlying mode switch, decodes to the listener and the
// hub, and the end of the decoder when the underlying mode is no longer
// allowed; nothing of a stopped session reaches the listener.
func TestDecoderSet(t *testing.T) {
	e := newDecoderEnv(t, true, 0, time.Minute)

	attach := e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "").result.(media.AttachResult)
	if d := attach.Device.Decoders; len(d) < 2 || d[0].Mode != "selcall" || !d[0].Available || d[0].Underlying[0] != "nfm" || d[1].Variants[0] != "ZVEI1" {
		t.Fatalf("device.config decoders %+v", d)
	}

	created := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "usb", "offset_hz": 12_500}, "").result.(media.DemodCreated)
	id := created.DemodID

	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": "d9", "decoder": "selcall"}, rxv1.CodeNotFound)
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "msk144"}, rxv1.CodeNotFound)
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "zvei", "options": map[string]any{"variant": "DTMF"}}, rxv1.CodeOutOfRange)

	// USB is not an underlying mode of SelCall: the demodulator switches
	// to NFM; the default variant is DTMF.
	started := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "").result.(media.DecoderStarted)
	if started.DecoderSessionID == "" || started.Variant != "DTMF" || started.Applied.Mode != "nfm" || started.Applied.Decoder == nil || *started.Applied.Decoder != "selcall" {
		t.Fatalf("decoder.set result %+v", started)
	}

	r := e.run.last()

	r.ev.Status(app.DecoderStatus{State: app.DecoderRunning})
	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_000_000), Schema: "selcall.v1", Text: "[DTMF] 1", Payload: json.RawMessage(`{"decoder":"DTMF","digits":"1"}`)})

	decodes := sentOf[media.Decode](e.p, rxv1.TypeDecode)
	if len(decodes) != 1 || decodes[0].Text != "[DTMF] 1" || decodes[0].FreqHz != 144_000_000+125_000+12_500 || decodes[0].DecoderSessionID != started.DecoderSessionID {
		t.Fatalf("decode %+v", decodes)
	}

	e.pub.mu.Lock()
	if len(e.pub.got) != 1 || e.pub.got[0].DeviceID != "vhf" || e.pub.got[0].Family != "paging" || e.pub.got[0].CID != "c1" || e.pub.got[0].SessionID != started.DecoderSessionID {
		t.Errorf("published %+v", e.pub.got)
	}
	e.pub.mu.Unlock()

	if st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState); len(st) != 1 || st[0].State != media.DecoderRunning || st[0].Variant != "DTMF" {
		t.Errorf("diag.state %+v", st)
	}

	// Another variant: a new session; the old one is stopped and silent.
	zvei := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "zvei", "options": map[string]any{"variant": "ZVEI2"}}, "").result.(media.DecoderStarted)
	if zvei.Variant != "ZVEI2" || e.run.last().variant != "ZVEI2" || !r.isClosed() {
		t.Fatalf("zvei %+v", zvei)
	}

	r.ev.Decode(app.DecodeRecord{Time: time.Now(), Schema: "selcall.v1", Text: "late", Payload: json.RawMessage(`{}`)})
	r.ev.Status(app.DecoderStatus{State: app.DecoderError, Reason: "crash"})

	if len(sentOf[media.Decode](e.p, rxv1.TypeDecode)) != 1 || e.pub.count() != 1 {
		t.Error("a stopped session still reports decodes")
	}

	st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState)
	if len(st) != 2 || st[1].State != media.DecoderStopped || st[1].Decoder != "selcall" {
		t.Errorf("diag.state after the switch %+v", st)
	}

	// AM is not allowed: the decoder stops.
	z := e.run.last()

	applied := e.check(rxv1.TypeDemodSet, map[string]any{"demod_id": id, "mode": "am"}, "").result.(media.AppliedResult)
	if applied.Applied.Decoder != nil || !z.isClosed() {
		t.Fatalf("decoder kept on AM: %+v", applied.Applied)
	}

	if st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState); st[len(st)-1].State != media.DecoderStopped {
		t.Errorf("diag.state %+v", st)
	}

	// Stopping explicitly.
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "zvei"}, "")
	z = e.run.last()

	stopped := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": nil}, "").result.(media.DecoderStarted)
	if stopped.DecoderSessionID != "" || stopped.Applied.Decoder != nil || !z.isClosed() {
		t.Fatalf("decoder.set null %+v", stopped)
	}

	// Removing the demodulator stops its decoder.
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "")
	last := e.run.last()

	e.check(rxv1.TypeDemodRemove, map[string]any{"demod_id": id}, "")

	if !last.isClosed() {
		t.Error("decoder kept after demod.remove")
	}
}

// decoder.set is rate limited per connection.
func TestDecoderSetRateLimit(t *testing.T) {
	e := newDecoderEnv(t, true, 0, 0)

	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, "").result.(media.DemodCreated).DemodID

	for range radiohttp.DecoderSetBurst {
		e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "")
	}

	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, rxv1.CodeRateLimited)
}

// Beyond decoders.max_sessions a decoder is unavailable (node busy).
func TestDecoderNodeBusy(t *testing.T) {
	e := newDecoderEnv(t, true, 1, time.Minute)

	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, "").result.(media.DemodCreated).DemodID

	// One session runs; replacing it frees its slot first.
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "")
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "zvei"}, "")
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": nil}, "")

	// Another listener's session holds the only slot.
	_, other, err := e.dec.Start(app.DecoderSpec{Mode: domain.DigitalModes()[0], Variant: "DTMF"}, func(shared.UUID) app.DecoderEvents { return app.DecoderEvents{} })
	if err != nil {
		t.Fatal(err)
	}

	res := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "").result.(media.DecoderStarted)
	st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState)

	if res.DecoderSessionID != "" || res.Applied.Decoder != nil || st[len(st)-1].State != media.DecoderUnavailable || st[len(st)-1].Reason != "node busy" {
		t.Fatalf("busy: %+v %+v", res, st)
	}

	other.Close()
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, "")
}

// A WSJT decoder sets its pass band on USB (WSPR: 1350 to 1650 Hz), its
// decodes are at the dial plus their audio frequency, and the clock
// warning reaches the listener (DEC-020, DEC-026, DEC-027).
func TestDecoderSetWSJT(t *testing.T) {
	e := newDecoderEnv(t, true, 0, time.Minute)
	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")

	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 12_500}, "").result.(media.DemodCreated).DemodID

	started := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "wspr"}, "").result.(media.DecoderStarted)
	if a := started.Applied; a.Mode != "usb" || a.Bandpass.LowHz != 1350 || a.Bandpass.HighHz != 1650 {
		t.Fatalf("applied %+v", a)
	}

	r := e.run.last()
	r.ev.Status(app.DecoderStatus{State: app.DecoderRunning, Warning: app.WarningClock})
	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_040_000), Schema: "wsjt.v1", Text: "K1ABC FN42 33", Payload: json.RawMessage(`{}`), AudioHz: 1500})

	if d := sentOf[media.Decode](e.p, rxv1.TypeDecode); len(d) != 1 || d[0].FreqHz != 144_000_000+125_000+12_500+1500 {
		t.Errorf("decode %+v", d)
	}

	// A slot decode carries the dial of its slot start; the events give
	// the current dial.
	if dial := r.ev.Dial(); dial != 144_000_000+125_000+12_500 {
		t.Errorf("dial %d", dial)
	}

	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_160_000), Schema: "wsjt.v1", Text: "K1ABC FN42 30", Payload: json.RawMessage(`{}`), AudioHz: 1500, DialHz: 10_138_700})

	if d := sentOf[media.Decode](e.p, rxv1.TypeDecode); len(d) != 2 || d[1].FreqHz != 10_140_200 {
		t.Errorf("slot decode %+v", d)
	}

	if st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState); len(st) != 1 || st[0].Warning != "clock_unsynced" {
		t.Errorf("diag.state %+v", st)
	}
}

// A digital mode whose tool is missing is refused; admins see why.
func TestDecoderUnavailable(t *testing.T) {
	for _, admin := range []bool{false, true} {
		var roles []string
		if admin {
			roles = []string{"admin"}
		}

		e := newDecoderEnv(t, false, 0, time.Minute, roles...)

		cfg := e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "").result.(media.AttachResult).Device
		want := "not available on this receiver"

		if admin {
			want = "multimon-ng not found"
		}

		if d := cfg.Decoders; len(d) < 2 || d[0].Available || d[0].Reason != want {
			t.Errorf("admin=%v: decoders %+v", admin, d)
		}

		id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, "").result.(media.DemodCreated).DemodID
		e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}, rxv1.CodeDemodError)
	}
}

// Image decoders (DEC-037, DEC-038): the image start is kept by the hub,
// the rows go to the listener only, and an image saved after the session
// ended is stamped with the reception of its start (FIL-008).
func TestDecoderImages(t *testing.T) {
	e := newDecoderEnv(t, true, 0, time.Minute)

	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 12_500}, "").result.(media.DemodCreated).DemodID

	started := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "sstv"}, "").result.(media.DecoderStarted)
	if started.Applied.Mode != "nfm" {
		t.Fatalf("SSTV switched NFM to %s", started.Applied.Mode)
	}

	r := e.run.last()

	// Without its start, an image has no reception: it is dropped.
	r.ev.File(app.ProducedFile{Kind: "sstv", Data: []byte("png"), Start: time.Now()})

	r.ev.Decode(app.DecodeRecord{Time: time.Now(), Schema: "image.v1", Text: "Robot 36 (VIS 8), 320×240", Payload: json.RawMessage(`{"event":"start"}`)})
	r.ev.Decode(app.DecodeRecord{Time: time.Now(), Schema: "image.v1", Payload: json.RawMessage(`{"event":"row","row":0}`), Live: true})

	if n := len(sentOf[media.Decode](e.p, rxv1.TypeDecode)); n != 2 || e.pub.count() != 1 {
		t.Fatalf("%d decodes sent, %d published", n, e.pub.count())
	}

	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": nil}, "")

	if !r.isClosed() {
		t.Fatal("decoder kept")
	}

	start := time.UnixMilli(1_800_000_000_000)
	r.ev.File(app.ProducedFile{Kind: "sstv", Data: []byte("png"), Start: start, Metadata: map[string]any{"vis_code": 8}})

	got := e.files.all()
	if len(got) != 1 {
		t.Fatalf("files %+v", got)
	}

	if f := got[0]; f.DeviceID != "vhf" || f.Mode != "sstv" || f.FreqHz != 144_000_000+125_000+12_500 || f.SessionID != started.DecoderSessionID || !f.Start.Equal(start) {
		t.Errorf("file %+v", f)
	}
}

// Wide IQ decoders (DEC-039, DEC-040): ISM fits the 250 kHz device and
// runs; WMBus needs 1.2 MS/s and is unavailable, with the reason. A live
// record (a skimmer's text) carries its signal offset and stays with the
// listener.
func TestDecoderWideIQ(t *testing.T) {
	e := newDecoderEnv(t, true, 0, time.Minute)

	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "am", "offset_hz": 0}, "").result.(media.DemodCreated).DemodID

	res := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "wmbus"}, "").result.(media.DecoderStarted)
	st := sentOf[media.DiagState](e.p, rxv1.TypeDiagState)

	if res.DecoderSessionID != "" || len(st) != 1 || st[0].State != media.DecoderUnavailable ||
		st[0].Reason != "the device sample rate (250000 Hz) is below the 1200000 Hz this decoder needs" {
		t.Fatalf("wmbus: %+v %+v", res, st)
	}

	started := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "ism"}, "").result.(media.DecoderStarted)
	if started.DecoderSessionID == "" || started.Applied.Mode != "am" {
		t.Fatalf("ism: %+v", started)
	}

	r := e.run.last()
	r.ev.Decode(app.DecodeRecord{Time: time.Now(), Schema: "skimmer.v1", Text: "x", Payload: json.RawMessage(`{}`), AudioHz: 1000, Live: true})

	d := sentOf[media.Decode](e.p, rxv1.TypeDecode)
	if len(d) != 1 || d[0].FreqHz != 144_000_000+125_000+1000 || e.pub.count() != 0 {
		t.Errorf("decodes %+v, published %d", d, e.pub.count())
	}

	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": nil}, "")

	if !r.isClosed() {
		t.Error("decoder kept")
	}
}
