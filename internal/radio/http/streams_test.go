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
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
)

type idleSource struct{}

func (idleSource) Run(ctx context.Context, _ domain.Tuning, _ app.IQSink, report func(app.SourceEvent)) error {
	report(app.SourceEvent{State: domain.StateStarting})
	report(app.SourceEvent{State: domain.StateRunning})
	<-ctx.Done()

	return nil
}

func (idleSource) SetCenter(int64) error { return nil }

type sources struct{}

func (sources) Probe(context.Context, domain.DeviceParams) error { return nil }
func (sources) New(domain.DeviceParams) (app.Source, error)      { return idleSource{}, nil }

type reply struct {
	typ    rxv1.MessageType
	code   rxv1.ErrorCode
	result any
}

type peer struct {
	mu      sync.Mutex
	replies []reply
	sent    []rxv1.MessageType
	q       *sendq.Queue
	claims  token.Claims
}

func (p *peer) Claims() token.Claims { return p.claims }
func (p *peer) Hello() media.Hello   { return media.Hello{} }
func (p *peer) Send(typ rxv1.MessageType, _ any) {
	p.mu.Lock()
	p.sent = append(p.sent, typ)
	p.mu.Unlock()
}

func (p *peer) Ack(_ rxv1.Envelope, result any) {
	p.mu.Lock()
	p.replies = append(p.replies, reply{typ: rxv1.TypeAck, result: result})
	p.mu.Unlock()
}

func (p *peer) Fail(_ rxv1.Envelope, code rxv1.ErrorCode, _ string) {
	p.mu.Lock()
	p.replies = append(p.replies, reply{typ: rxv1.TypeError, code: code})
	p.mu.Unlock()
}

func (p *peer) Queue() *sendq.Queue { return p.q }

func (p *peer) last() reply {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.replies[len(p.replies)-1]
}

func env(t *testing.T, typ rxv1.MessageType, payload any) rxv1.Envelope {
	t.Helper()

	b, _ := json.Marshal(map[string]any{"v": 1, "type": typ, "id": "x", "ts": 1, "payload": payload})

	e, err := rxv1.DecodeEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}

	return e
}

func TestStreamSessionErrors(t *testing.T) {
	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)
	drv, _ := domain.NewDriver(typ, "0", 0, domain.AutoGain(), false)
	r, _ := domain.NewFreqRange(domain.MustFrequency(144_000_000), domain.MustFrequency(146_000_000))
	dev, _ := domain.NewDevice(domain.DeviceParams{
		ID: domain.MustDeviceID("vhf"), Name: "VHF", Type: typ, Enabled: true, Range: r,
		Rates: []domain.SampleRate{domain.MustSampleRate(250_000)}, Driver: drv,
	})

	m, err := app.NewManager(app.Options{
		Devices: []*domain.Device{dev}, Sources: sources{}, Engines: engine.Factory{Logger: slog.New(slog.DiscardHandler)},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.Run(ctx)
		close(done)
	}()

	defer func() {
		cancel()
		<-done
	}()

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: token.Claims{Limits: token.Limits{MaxDemods: 1}}}
	ss := radiohttp.NewStreams(m, slog.New(slog.DiscardHandler)).Open(p)

	defer ss.Close()

	check := func(typ rxv1.MessageType, payload any, want rxv1.ErrorCode) {
		t.Helper()
		ss.Handle(ctx, env(t, typ, payload))

		got := p.last()
		if want == "" && got.typ != rxv1.TypeAck || want != "" && got.code != want {
			t.Fatalf("%s %v: got %+v, want %q", typ, payload, got, want)
		}
	}

	check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "nope"}, rxv1.CodeNotFound)
	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, rxv1.CodeConflict)
	check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf", "fft": map[string]any{"codec": "f32-db"}}, rxv1.CodeOutOfRange)
	check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf", "fft": map[string]any{"codec": "u8-db", "fps": 5}}, "")
	if res, ok := p.last().result.(media.AttachResult); !ok || len(res.Streams) != 1 || res.Streams[0].FPS != 5 || res.Device.Permissions.Retune {
		t.Fatalf("attach result %+v", p.last().result)
	}

	check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, rxv1.CodeConflict)

	check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": 99}, rxv1.CodeNotFound)
	check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": 1, "fps": 50}, rxv1.CodeOutOfRange)
	check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": 1, "paused": true}, "")
	check(rxv1.TypeAudioConfigure, map[string]any{"codec": "pcm-s16le", "sample_rate": 9000}, rxv1.CodeOutOfRange)
	check(rxv1.TypeAudioConfigure, map[string]any{"codec": "opus", "sample_rate": 48000}, "")

	if res := p.last().result.(media.AudioConfigure); res.Codec != media.CodecADPCM {
		t.Fatalf("opus not refused for adpcm: %+v", res)
	}

	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "am"}, rxv1.CodeDemodError)
	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 500_000}, rxv1.CodeOutOfRange)
	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 10_000}, "")

	created := p.last().result.(media.DemodCreated)

	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, rxv1.CodeCapacityExceeded)
	check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "offset_hz": -20_000}, "")
	check(rxv1.TypeDemodSet, map[string]any{"demod_id": "zz"}, rxv1.CodeNotFound)
	check(rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 10}, rxv1.CodeOutOfRange)
	check(rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_500_000}, "")
	check(rxv1.TypeDemodRemove, map[string]any{"demod_id": created.DemodID}, "")
	check(rxv1.TypeDeviceDetach, map[string]any{"device_id": "vhf"}, "")
	check(rxv1.TypeDeviceDetach, map[string]any{"device_id": "vhf"}, rxv1.CodeNotFound)
	check(rxv1.TypeBye, map[string]any{}, rxv1.CodeUnsupportedType)

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.sent) == 0 || p.sent[0] != rxv1.TypeDeviceConfig {
		t.Fatalf("sent %v", p.sent)
	}
}
