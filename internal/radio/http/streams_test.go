package http_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	radiohttp "github.com/yohang/mesh-sdr/internal/radio/http"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
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
	payload []any
	q       *sendq.Queue
	claims  token.Claims
	hello   media.Hello
}

func (p *peer) Claims() token.Claims {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.claims
}

func (p *peer) setClaims(c token.Claims) {
	p.mu.Lock()
	p.claims = c
	p.mu.Unlock()
}

func (p *peer) count(typ rxv1.MessageType) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0

	for _, s := range p.sent {
		if s == typ {
			n++
		}
	}

	return n
}
func (p *peer) Hello() media.Hello { return p.hello }
func (p *peer) Send(typ rxv1.MessageType, payload any) {
	p.mu.Lock()
	p.sent = append(p.sent, typ)
	p.payload = append(p.payload, payload)
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

func (p *peer) RateLimited(rxv1.Envelope, time.Duration) {
	p.mu.Lock()
	p.replies = append(p.replies, reply{typ: rxv1.TypeError, code: rxv1.CodeRateLimited})
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

func scoped(maxDemods int, perms ...string) token.Claims {
	return token.Claims{Limits: token.Limits{MaxDemods: maxDemods}, Scopes: []token.Scope{{Device: "vhf", Perms: perms}}}
}

// runManager runs a manager with one vhf device on an idle source.
func runManager(t *testing.T) *app.Manager {
	t.Helper()

	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)
	drv, _ := domain.NewDriver(typ, domain.DriverSettings{Device: "0", Gain: domain.AutoGain()})
	r, _ := domain.NewFreqRange(domain.MustFrequency(144_000_000), domain.MustFrequency(146_000_000))
	dev, _ := domain.NewDevice(domain.DeviceParams{
		ID: shared.MustDeviceID("vhf"), Name: "VHF", Type: typ, Enabled: true, Range: r,
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

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return m
}

func TestStreamSessionErrors(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen, token.PermDemod)}
	ss := radiohttp.NewStreams(m, nil, slog.New(slog.DiscardHandler)).Open(p)

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
	check(rxv1.TypeAudioConfigure, map[string]any{"codec": "vorbis", "sample_rate": 48000}, rxv1.CodeOutOfRange)
	check(rxv1.TypeAudioConfigure, map[string]any{"codec": "opus", "sample_rate": 48000}, "")

	if res := p.last().result.(media.AudioConfigure); res.Codec != media.CodecADPCM {
		t.Fatalf("opus not refused for adpcm: %+v", res)
	}

	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "dmr"}, rxv1.CodeDemodError)
	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 500_000}, rxv1.CodeOutOfRange)
	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 10_000}, "")

	created := p.last().result.(media.DemodCreated)

	check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, rxv1.CodeCapacityExceeded)
	check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "offset_hz": -20_000}, "")

	// A new mode takes its default pass band; NR is range-checked; broadcast
	// FM runs on the 48 kHz audio configured above.
	check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "mode": "usb", "nr": map[string]any{"enabled": true, "threshold": 6}}, "")

	if a := p.last().result.(media.AppliedResult).Applied; a.Bandpass != (media.Bandpass{LowHz: 300, HighHz: 2700}) || !a.NR.Enabled || a.NR.Threshold != 6 {
		t.Fatalf("applied %+v", a)
	}

	check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "nr": map[string]any{"enabled": true, "threshold": 30}}, rxv1.CodeOutOfRange)
	check(rxv1.TypeDemodSet, map[string]any{"demod_id": created.DemodID, "mode": "wfm"}, "")
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

func TestReauthorizeAfterRefresh(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(2, token.PermListen, token.PermDemod)}
	ss := radiohttp.NewStreams(m, nil, slog.New(slog.DiscardHandler)).Open(p)

	defer ss.Close()

	ss.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))
	ss.Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}))
	ss.Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 5000}))

	if got := p.last(); got.typ != rxv1.TypeAck {
		t.Fatalf("setup: %+v", got)
	}

	// A lower demodulator limit removes the newest one.
	p.setClaims(scoped(1, token.PermListen, token.PermDemod))
	ss.Reauthorize()

	if n := p.count(rxv1.TypeStreamClose); n != 1 {
		t.Fatalf("%d streams closed, want 1", n)
	}

	ss.Handle(ctx, env(t, rxv1.TypeDemodSet, map[string]any{"demod_id": "d2", "offset_hz": 1000}))

	if got := p.last(); got.code != rxv1.CodeNotFound {
		t.Fatalf("removed demod still set: %+v", got)
	}

	// Losing demod removes the other one; losing listen detaches.
	p.setClaims(scoped(1, token.PermListen))
	ss.Handle(ctx, env(t, rxv1.TypeDemodSet, map[string]any{"demod_id": "d1", "offset_hz": 1000}))

	if got := p.last(); got.code != rxv1.CodeForbidden {
		t.Fatalf("demod.set without scope: %+v", got)
	}

	ss.Reauthorize()

	if n := p.count(rxv1.TypeStreamClose); n != 2 {
		t.Fatalf("%d streams closed, want 2", n)
	}

	p.setClaims(token.Claims{})
	ss.Reauthorize()

	if n := p.count(rxv1.TypeStreamClose); n != 3 {
		t.Fatalf("%d streams closed, want 3 (FFT)", n)
	}

	ss.Handle(ctx, env(t, rxv1.TypeDeviceDetach, map[string]any{"device_id": "vhf"}))

	if got := p.last(); got.code != rxv1.CodeNotFound {
		t.Fatalf("device still attached: %+v", got)
	}
}

// lastSent returns the payload of the last message of a type.
func (p *peer) lastSent(typ rxv1.MessageType) any {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := len(p.sent) - 1; i >= 0; i-- {
		if p.sent[i] == typ {
			return p.payload[i]
		}
	}

	return nil
}

// desired is a desired state pushed by the hub.
type desired struct {
	devices map[string]ctl.DesiredDevice
	presets map[string]ctl.Preset
	policy  ctl.StatePolicy
}

func (d desired) Device(id string) (ctl.DesiredDevice, bool) { v, ok := d.devices[id]; return v, ok }
func (d desired) Preset(id string) (ctl.Preset, bool)        { v, ok := d.presets[id]; return v, ok }
func (d desired) Policy() ctl.StatePolicy                    { return d.policy }

// TestPresetSelect: preset.select retunes the shared device, moves the
// caller's demodulator to the preset's start, keeps the other listener's
// frequency and sends device.config to both (shared centre). Without the
// retune right, a switch that would leave another listener's demodulator
// outside the new band is refused.
func TestPresetSelect(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	squelch, nr := -60, 6
	state := desired{
		devices: map[string]ctl.DesiredDevice{"vhf": {Presets: []string{"near", "far"}}},
		presets: map[string]ctl.Preset{
			// The device starts at 144.125 MHz (250 kS/s).
			"near": {Name: "Near", CenterFreq: 144_150_000, SampRate: 250_000, StartFreq: 144_160_000, StartMod: "nfm", TuningStep: 12_500, InitialSquelchLevel: &squelch, InitialNRLevel: &nr},
			"far":  {Name: "Far", CenterFreq: 145_000_000, SampRate: 250_000, StartFreq: 145_000_000, StartMod: "nfm", TuningStep: 1000},
			"hf":   {Name: "HF", CenterFreq: 14_074_000, SampRate: 250_000, StartFreq: 14_074_000, StartMod: "usb", TuningStep: 1000},
		},
		policy: ctl.StatePolicy{ListenPolicy: "anonymous", Waterfall: &ctl.StateWaterfall{MinDB: -100, MaxDB: -30, Palette: "default"}},
	}
	streams := radiohttp.NewStreams(m, state, slog.New(slog.DiscardHandler))
	now := time.Now()
	streams.SetNow(func() time.Time { return now })

	newPeer := func(perms ...string) *peer {
		return &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, perms...)}
	}

	op := newPeer(token.PermListen, token.PermDemod, token.PermPreset)
	lis := newPeer(token.PermListen, token.PermDemod)
	opSS, lisSS := streams.Open(op), streams.Open(lis)

	defer opSS.Close()
	defer lisSS.Close()

	do := func(ss media.StreamSession, p *peer, typ rxv1.MessageType, payload any, want rxv1.ErrorCode) reply {
		t.Helper()
		ss.Handle(ctx, env(t, typ, payload))

		got := p.last()
		if want == "" && got.typ != rxv1.TypeAck || want != "" && got.code != want {
			t.Fatalf("%s %v: got %+v, want %q", typ, payload, got, want)
		}

		return got
	}

	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "near"}, rxv1.CodeConflict)

	att := do(opSS, op, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	if cfg := att.result.(media.AttachResult).Device; len(cfg.PresetsAvailable) != 2 || cfg.ActivePreset != nil ||
		cfg.Waterfall.Levels != (media.Levels{Min: -100, Max: -30}) || cfg.Waterfall.Scheme != "default" || !cfg.Permissions.Preset {
		t.Fatalf("device.config before a switch: %+v", cfg)
	}

	do(opSS, op, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}, "")
	do(lisSS, lis, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	do(lisSS, lis, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 20_000}, "") // 144.145 MHz

	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "zz"}, rxv1.CodeNotFound)
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "hf"}, rxv1.CodePresetIncompatible)

	// The retune gate: far would leave the listener outside the band.
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "far"}, rxv1.CodeForbidden)

	if s := m.Devices()[0]; s.CenterHz != 144_125_000 {
		t.Fatalf("a refused switch retuned the device: %d", s.CenterHz)
	}

	res := do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "near"}, "")
	if res.result.(media.PresetSelected).ActivePresetID != "near" || m.Devices()[0].CenterHz != 144_150_000 ||
		m.Devices()[0].ActivePreset != "near" {
		t.Fatalf("switch: %+v, device %+v", res.result, m.Devices()[0])
	}

	applied := func(p *peer) media.Applied {
		t.Helper()

		u, ok := p.lastSent(rxv1.TypeStreamUpdate).(media.StreamUpdate)
		if !ok || u.Applied == nil {
			t.Fatalf("no demodulator update: %+v", p.lastSent(rxv1.TypeStreamUpdate))
		}

		return *u.Applied
	}

	// The caller starts at the preset; the listener keeps 144.145 MHz.
	if a := applied(op); a.OffsetHz != 10_000 || a.Mode != "nfm" || a.SquelchDB == nil || *a.SquelchDB != -60 ||
		!a.NR.Enabled || a.NR.Threshold != 6 {
		t.Errorf("caller demodulator: %+v", a)
	}

	if a := applied(lis); a.OffsetHz != -5_000 {
		t.Errorf("listener demodulator: %+v", a)
	}

	for name, p := range map[string]*peer{"caller": op, "listener": lis} {
		cfg, ok := p.lastSent(rxv1.TypeDeviceConfig).(media.DeviceConfig)
		if !ok || cfg.ActivePreset == nil || cfg.ActivePreset.ID != "near" || cfg.CenterHz != 144_150_000 || cfg.Revision < 1 ||
			cfg.Start.OffsetHz != 10_000 || cfg.TuningStepHz != 12_500 || cfg.Squelch.Initial != -60 {
			t.Errorf("%s device.config: %+v", name, cfg)
		}
	}

	// With the retune right the switch goes through; the listener's
	// demodulator, now outside the band, moves to the preset's start.
	op.setClaims(scoped(1, token.PermListen, token.PermDemod, token.PermPreset, token.PermRetune))

	// One switch per device every 5 s (§5.11).
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "far"}, rxv1.CodeRateLimited)

	now = now.Add(radiohttp.PresetSwitchEvery)
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "far"}, "")

	if a := applied(lis); a.OffsetHz != 0 {
		t.Errorf("listener demodulator after a forced switch: %+v", a)
	}

	if cfg := lis.lastSent(rxv1.TypeDeviceConfig).(media.DeviceConfig); cfg.ActivePreset.ID != "far" || cfg.CenterHz != 145_000_000 {
		t.Errorf("listener device.config after a forced switch: %+v", cfg)
	}

	// The hub's state unchanged keeps the active preset; an edit of the
	// preset clears it.
	streams.StateApplied()

	if got := m.Devices()[0].ActivePreset; got != "far" {
		t.Fatalf("active preset after an unchanged state: %q", got)
	}

	far := state.presets["far"]
	far.TuningStep = 500
	state.presets["far"] = far
	streams.StateApplied()

	if got := m.Devices()[0].ActivePreset; got != "" {
		t.Errorf("active preset after an edit: %q", got)
	}

	// device.retune: the listener keeps 145 MHz while it stays in the band,
	// then moves to the new centre; the retune clears the active preset.
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "far"}, rxv1.CodeRateLimited)

	now = now.Add(radiohttp.PresetSwitchEvery)
	do(opSS, op, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "far"}, "")
	do(opSS, op, rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_050_000}, "")

	if a := applied(lis); a.OffsetHz != -50_000 {
		t.Errorf("listener demodulator after a retune: %+v", a)
	}

	if got := m.Devices()[0].ActivePreset; got != "" {
		t.Errorf("active preset after a retune: %q", got)
	}

	if cfg := lis.lastSent(rxv1.TypeDeviceConfig).(media.DeviceConfig); cfg.ActivePreset != nil || cfg.CenterHz != 145_050_000 {
		t.Errorf("listener device.config after a retune: %+v", cfg)
	}

	do(opSS, op, rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_500_000}, "")

	if a := applied(lis); a.OffsetHz != 0 {
		t.Errorf("listener demodulator after a retune out of its band: %+v", a)
	}

	// Four retunes per second (§5.11).
	do(opSS, op, rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_400_000}, "")
	do(opSS, op, rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_300_000}, "")
	do(opSS, op, rxv1.TypeDeviceRetune, map[string]any{"device_id": "vhf", "center_hz": 145_200_000}, rxv1.CodeRateLimited)
}

// TestPresetSelectRace: two operators switch the same device at once while
// a listener retunes its demodulator: one switch wins, the other is rate
// limited, and the listener's demodulator stays consistent (run with
// -race).
func TestPresetSelectRace(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	state := desired{
		devices: map[string]ctl.DesiredDevice{"vhf": {Presets: []string{"a", "b"}}},
		presets: map[string]ctl.Preset{
			"a": {Name: "A", CenterFreq: 144_150_000, SampRate: 250_000, StartFreq: 144_150_000, StartMod: "nfm", TuningStep: 1000},
			"b": {Name: "B", CenterFreq: 144_160_000, SampRate: 250_000, StartFreq: 144_160_000, StartMod: "nfm", TuningStep: 1000},
		},
		policy: ctl.StatePolicy{ListenPolicy: "anonymous"},
	}
	streams := radiohttp.NewStreams(m, state, slog.New(slog.DiscardHandler))

	all := []string{token.PermListen, token.PermDemod, token.PermPreset, token.PermRetune}
	peers := make([]*peer, 3)
	sessions := make([]media.StreamSession, 3)

	for i := range peers {
		peers[i] = &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, all...)}
		sessions[i] = streams.Open(peers[i])

		defer sessions[i].Close()

		sessions[i].Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))
		sessions[i].Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}))
	}

	var wg sync.WaitGroup

	for i, id := range []string{"a", "b"} {
		wg.Go(func() {
			sessions[i].Handle(ctx, env(t, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": id}))
		})
	}

	wg.Go(func() {
		for off := range 20 {
			sessions[2].Handle(ctx, env(t, rxv1.TypeDemodSet, map[string]any{"demod_id": "d1", "offset_hz": off * 1000}))
		}
	})
	wg.Wait()

	codes := map[rxv1.ErrorCode]int{}
	winner := ""

	for i, id := range []string{"a", "b"} {
		r := peers[i].last()
		codes[r.code]++

		if r.typ == rxv1.TypeAck {
			winner = id
		}
	}

	if codes[""] != 1 || codes[rxv1.CodeRateLimited] != 1 {
		t.Fatalf("switch outcomes: %v", codes)
	}

	if s := m.Devices()[0]; s.ActivePreset != winner || s.CenterHz != state.presets[winner].CenterFreq {
		t.Errorf("device after the race: %+v, winner %s", s, winner)
	}

	if r := peers[2].last(); r.typ != rxv1.TypeAck {
		t.Errorf("last demod.set: %+v", r)
	}
}

// TestPresetSelectUnknownMode: a start mode the engine refuses still moves
// the caller's demodulator to the preset's start, in its current mode.
func TestPresetSelectUnknownMode(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	state := desired{
		devices: map[string]ctl.DesiredDevice{"vhf": {Presets: []string{"odd"}}},
		presets: map[string]ctl.Preset{
			"odd": {Name: "Odd", CenterFreq: 144_150_000, SampRate: 250_000, StartFreq: 144_160_000, StartMod: "dmr", TuningStep: 1000},
		},
		policy: ctl.StatePolicy{ListenPolicy: "anonymous"},
	}

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen, token.PermDemod, token.PermPreset)}
	ss := radiohttp.NewStreams(m, state, slog.New(slog.DiscardHandler)).Open(p)

	defer ss.Close()

	ss.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))
	ss.Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}))
	ss.Handle(ctx, env(t, rxv1.TypePresetSelect, map[string]any{"device_id": "vhf", "preset_id": "odd"}))

	if r := p.last(); r.typ != rxv1.TypeAck {
		t.Fatalf("switch: %+v", r)
	}

	u, ok := p.lastSent(rxv1.TypeStreamUpdate).(media.StreamUpdate)
	if !ok || u.Applied == nil || u.Applied.OffsetHz != 10_000 || u.Applied.Mode != "nfm" {
		t.Errorf("demodulator after a switch to an unknown mode: %+v", u.Applied)
	}
}

// TestPCMHello: a client listing pcm-s16le alone (audio_compression = pcm)
// gets PCM audio (DEM-010).
func TestPCMHello(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen, token.PermDemod)}
	p.hello.Capabilities.AudioCodecs = []string{media.CodecPCM}
	ss := radiohttp.NewStreams(m, nil, slog.New(slog.DiscardHandler)).Open(p)

	defer ss.Close()

	ss.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))
	ss.Handle(ctx, env(t, rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm"}))

	if open, ok := p.lastSent(rxv1.TypeStreamOpen).(media.StreamOpen); !ok || open.Kind != media.KindAudio || open.Codec != media.CodecPCM {
		t.Fatalf("audio stream = %+v", p.lastSent(rxv1.TypeStreamOpen))
	}
}

// TestWaterfallLive: new waterfall settings in the desired state reach the
// attached listeners as a device.config.patch; an unchanged state sends
// nothing.
func TestWaterfallLive(t *testing.T) {
	m := runManager(t)
	ctx := context.Background()

	state := &desired{policy: ctl.StatePolicy{Waterfall: &ctl.StateWaterfall{MinDB: -100, MaxDB: -30, Palette: "default"}}}
	streams := radiohttp.NewStreams(m, state, slog.New(slog.DiscardHandler))

	p := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen)}
	ss := streams.Open(p)

	defer ss.Close()

	ss.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))

	streams.StateApplied()

	if n := p.count(rxv1.TypeDeviceConfigPatch); n != 0 {
		t.Fatalf("unchanged state sent %d patches", n)
	}

	state.policy.Waterfall = &ctl.StateWaterfall{MinDB: -90, MaxDB: -10, Palette: "turbo"}
	streams.StateApplied()

	patch, ok := p.lastSent(rxv1.TypeDeviceConfigPatch).(media.DeviceConfigPatch)
	if !ok || patch.DeviceID != "vhf" || patch.Revision != 1 ||
		patch.Set["waterfall"] != (media.Waterfall{Levels: media.Levels{Min: -90, Max: -10}, AutoMinRange: 50, Scheme: "turbo"}) {
		t.Fatalf("patch = %+v", p.lastSent(rxv1.TypeDeviceConfigPatch))
	}

	// The next device.config carries them too.
	p2 := &peer{q: sendq.New(sendq.DefaultConfig(), time.Now, nil), claims: scoped(1, token.PermListen)}
	ss2 := streams.Open(p2)

	defer ss2.Close()

	ss2.Handle(ctx, env(t, rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}))

	if cfg, ok := p2.lastSent(rxv1.TypeDeviceConfig).(media.DeviceConfig); !ok || cfg.Waterfall.Scheme != "turbo" || cfg.Waterfall.Levels.Min != -90 {
		t.Fatalf("device.config = %+v", p2.lastSent(rxv1.TypeDeviceConfig))
	}
}
