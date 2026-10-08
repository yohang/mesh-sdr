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
)

// tools reports cap:multimon-ng as ok or missing.
type tools struct{ ok bool }

func (t tools) Available(c string) (bool, string) {
	if c == domain.CapMultimonNG && t.ok {
		return true, ""
	}

	return false, "multimon-ng not found"
}

// fakeRun records its audio and whether it was closed.
type fakeRun struct {
	mu     sync.Mutex
	ev     app.DecoderEvents
	mode   string
	audio  int
	closed bool
}

func (r *fakeRun) Audio(b app.AudioBlock) {
	r.mu.Lock()
	r.audio += len(b.Samples)
	r.mu.Unlock()
}

func (r *fakeRun) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

type runner struct {
	mu   sync.Mutex
	runs []*fakeRun
}

func (r *runner) Start(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	run := &fakeRun{ev: ev, mode: spec.Mode.Name}

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

// TestDecoderSet covers decoder.set (DEC-002, DEC-003): the allow-list,
// the underlying mode switch, decodes to the listener and the hub, and the
// end of the decoder when the underlying mode is no longer allowed.
func TestDecoderSet(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()
	run := &runner{}
	pub := &publisher{}
	dec := radiohttp.Decoding{Decoders: app.NewDecoders(tools{ok: true}, run, time.Now), Publisher: pub}

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen, token.PermDemod)}
	p.claims.ConnectionID = "c1"
	ss := radiohttp.NewStreams(m, nil, dec, slog.New(slog.DiscardHandler)).Open(p)

	defer ss.Close()

	check := func(typ rxv1.MessageType, payload any, want rxv1.ErrorCode) reply {
		t.Helper()
		ss.Handle(ctx, env(t, typ, payload))

		got := p.last()
		if want == "" && got.typ != rxv1.TypeAck || want != "" && got.code != want {
			t.Fatalf("%s %v: got %+v, want %q", typ, payload, got, want)
		}

		return got
	}

	attach := check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "").result.(media.AttachResult)
	if d := attach.Device.Decoders; len(d) != 2 || d[0].Mode != "selcall" || !d[0].Available || d[0].Underlying[0] != "nfm" {
		t.Fatalf("device.config decoders %+v", d)
	}

	created := check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "usb", "offset_hz": 12_500}, "").result.(media.DemodCreated)

	check(rxv1.TypeDecoderSet, map[string]any{"demod_id": "d9", "decoder": "selcall"}, rxv1.CodeNotFound)
	check(rxv1.TypeDecoderSet, map[string]any{"demod_id": created.DemodID, "decoder": "ft8"}, rxv1.CodeNotFound)

	// USB is not an underlying mode of SelCall: the demodulator switches
	// to NFM.
	started := check(rxv1.TypeDecoderSet, map[string]any{"demod_id": created.DemodID, "decoder": "selcall"}, "").result.(media.DecoderStarted)
	if started.DecoderSessionID == "" || started.Applied.Mode != "nfm" || started.Applied.Decoder == nil || *started.Applied.Decoder != "selcall" {
		t.Fatalf("decoder.set result %+v", started)
	}

	r := run.last()

	r.ev.Status(app.DecoderStatus{State: app.DecoderRunning})
	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_000_000), Schema: "selcall.v1", Text: "[DTMF] 1", Payload: json.RawMessage(`{"decoder":"DTMF","digits":"1"}`)})

	decodes := sentOf[media.Decode](p, rxv1.TypeDecode)
	if len(decodes) != 1 || decodes[0].Text != "[DTMF] 1" || decodes[0].FreqHz != 144_000_000+125_000+12_500 || decodes[0].DecoderSessionID != started.DecoderSessionID {
		t.Fatalf("decode %+v", decodes)
	}

	pub.mu.Lock()
	if len(pub.got) != 1 || pub.got[0].DeviceID != "vhf" || pub.got[0].Family != "paging" || pub.got[0].CID != "c1" || pub.got[0].SessionID != started.DecoderSessionID {
		t.Errorf("published %+v", pub.got)
	}
	pub.mu.Unlock()

	if st := sentOf[media.DiagState](p, rxv1.TypeDiagState); len(st) != 1 || st[0].State != media.DecoderRunning {
		t.Errorf("diag.state %+v", st)
	}

	// AM is not allowed: the decoder stops.
	applied := check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "mode": "am"}, "").result.(media.AppliedResult)
	if applied.Applied.Decoder != nil || !r.closed {
		t.Fatalf("decoder kept on AM: %+v closed=%v", applied.Applied, r.closed)
	}

	if st := sentOf[media.DiagState](p, rxv1.TypeDiagState); len(st) != 2 || st[1].State != media.DecoderStopped {
		t.Errorf("diag.state %+v", st)
	}

	// Stopping explicitly.
	check(rxv1.TypeDecoderSet, map[string]any{"demod_id": created.DemodID, "decoder": "zvei"}, "")
	z := run.last()

	stopped := check(rxv1.TypeDecoderSet, map[string]any{"demod_id": created.DemodID, "decoder": nil}, "").result.(media.DecoderStarted)
	if stopped.DecoderSessionID != "" || stopped.Applied.Decoder != nil || !z.closed {
		t.Fatalf("decoder.set null %+v", stopped)
	}

	// Removing the demodulator stops its decoder.
	check(rxv1.TypeDecoderSet, map[string]any{"demod_id": created.DemodID, "decoder": "selcall"}, "")
	last := run.last()

	check(rxv1.TypeDemodRemove, map[string]any{"demod_id": created.DemodID}, "")

	if !last.closed {
		t.Error("decoder kept after demod.remove")
	}
}

// A digital mode whose tool is missing is refused; admins see why.
func TestDecoderUnavailable(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()
	dec := radiohttp.Decoding{Decoders: app.NewDecoders(tools{}, &runner{}, time.Now)}

	for _, admin := range []bool{false, true} {
		claims := scoped(1, token.PermListen, token.PermDemod)
		if admin {
			claims.Roles = []string{"admin"}
		}

		p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: claims}
		ss := radiohttp.NewStreams(m, nil, dec, slog.New(slog.DiscardHandler)).Open(p)

		ss.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))

		cfg := p.last().result.(media.AttachResult).Device
		want := "not available on this receiver"

		if admin {
			want = "multimon-ng not found"
		}

		if d := cfg.Decoders; len(d) != 2 || d[0].Available || d[0].Reason != want {
			t.Errorf("admin=%v: decoders %+v", admin, d)
		}

		ss.Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}))
		id := p.last().result.(media.DemodCreated).DemodID

		ss.Handle(ctx, env(t, rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall"}))

		if got := p.last(); got.code != rxv1.CodeDemodError {
			t.Errorf("admin=%v: %+v", admin, got)
		}

		ss.Close()
	}
}
