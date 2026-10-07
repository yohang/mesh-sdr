package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

func TestMain(m *testing.M) {
	// Nodes started by these tests spawn connectors through the exec
	// helper, which is the test binary itself.
	process.MaybeRunExecHelper()

	code := m.Run()

	if fakeConnectorDir != "" {
		_ = os.RemoveAll(fakeConnectorDir)
	}

	os.Exit(code)
}

var (
	fakeConnectorOnce sync.Once
	fakeConnectorDir  string
	errFakeConnector  error
)

// fakeConnector builds the fake owrx connector once per test binary.
func fakeConnector(t *testing.T) string {
	t.Helper()

	fakeConnectorOnce.Do(func() {
		fakeConnectorDir, errFakeConnector = os.MkdirTemp("", "wire-tools-")
		if errFakeConnector != nil {
			return
		}

		out, err := exec.Command("go", "build", "-o", filepath.Join(fakeConnectorDir, "rtl_connector"),
			"github.com/yohang/mesh-sdr/internal/radio/infra/connector/fakeconnector").CombinedOutput()
		if err != nil {
			errFakeConnector = fmt.Errorf("build fake connector: %w: %s", err, out)
		}
	})

	if errFakeConnector != nil {
		t.Fatal(errFakeConnector)
	}

	return filepath.Join(fakeConnectorDir, "rtl_connector")
}

type mediaClient struct {
	t  *testing.T
	ws *websocket.Conn
	n  int
}

func (c *mediaClient) send(typ string, payload any) string {
	c.t.Helper()

	c.n++
	id := "r" + strconv.Itoa(c.n)

	b, err := json.Marshal(map[string]any{"v": 1, "type": typ, "id": id, "ts": time.Now().UnixMilli(), "payload": payload})
	if err != nil {
		c.t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}

	return id
}

// until reads messages until pred accepts one (text: envelope; binary:
// header), within 10 s.
func (c *mediaClient) until(what string, pred func(env *rxv1.Envelope, h *rxv1.FrameHeader, payload []byte) bool) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		typ, b, err := c.ws.Read(ctx)
		if err != nil {
			c.t.Fatalf("%s: %v", what, err)
		}

		if typ == websocket.MessageText {
			env, err := rxv1.DecodeEnvelope(b)
			if err != nil {
				c.t.Fatalf("%s: %v", what, err)
			}

			if env.Type() == rxv1.TypeError {
				c.t.Fatalf("%s: error %s", what, env.Payload())
			}

			if pred(&env, nil, nil) {
				return
			}

			continue
		}

		h, p, err := rxv1.ParseFrame(b)
		if err != nil {
			c.t.Fatalf("%s: %v", what, err)
		}

		if pred(nil, &h, p) {
			return
		}
	}
}

func isAck(id string) func(*rxv1.Envelope, *rxv1.FrameHeader, []byte) bool {
	return func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		if env == nil || env.Type() != rxv1.TypeAck {
			return false
		}

		var p struct {
			Re string `json:"re"`
		}

		_ = json.Unmarshal(env.Payload(), &p)

		return p.Re == id
	}
}

// TestMediaDeviceStreaming: a node with an rtl_sdr device (fake connector)
// serves the waterfall and NFM audio over its media WebSocket: the device
// starts on attach, FFT and audio frames flow in rx.v1, the operator
// retunes it, and the hub registry follows its state.
func TestMediaDeviceStreaming(t *testing.T) {
	e := newGridEnv(t, fastTimings())

	tool := fakeConnector(t)
	e.nodeCfg.Node.RuntimeDir = filepath.Join(t.TempDir(), "run")
	e.nodeCfg.Tools.RTLConnector = tool
	e.nodeCfg.Devices = map[string]config.DeviceConfig{"vhf": {
		Name: "VHF", Type: "rtl_sdr", FreqRange: config.FreqRange{Min: config.MustFrequency("144MHz"), Max: config.MustFrequency("146MHz")},
		SampleRates: []int64{250_000}, ListenPolicy: "anonymous", OperatorCanRetune: true,
	}}

	e.enrollNode(t, fakeProber{})

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	now := time.Now()
	tok := func(cid string) string {
		raw, err := e.g.keys.Issue(context.Background(), token.Claims{
			Issuer: e.hubCfg.Hub.URL, Audience: token.Audience("attic"), Subject: "u1", SessionID: token.SessionRef("s1"),
			ConnectionID: cid, Roles: []string{"operator"}, IssuedAt: now.Add(-time.Second), NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
			Scopes: []token.Scope{{Device: "vhf", Perms: []string{token.PermListen, token.PermDemod, token.PermRetune}}},
			Limits: token.Limits{MaxDemods: 2},
		})
		if err != nil {
			t.Fatal(err)
		}

		return raw
	}

	var ws *websocket.Conn

	eventually(t, "keys installed", 5*time.Second, func() bool {
		var st int
		ws, st = e.dialMedia(t, tok("m1"), "m1")

		return ws != nil && st == 101
	})

	ws.SetReadLimit(1 << 20)

	c := &mediaClient{t: t, ws: ws}
	c.send("session.hello", map[string]any{"client": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{
		"audio_codecs": []string{"adpcm-ima"}, "fft_codecs": []string{"u8-db"}, "audio_rates": []int{12000}, "max_fft_fps": 10,
	}})
	c.until("welcome", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		return env != nil && env.Type() == rxv1.TypeSessionWelcome
	})

	id := c.send("time.sync", map[string]any{"t0": 1})
	c.until("time.sync.reply", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		return env != nil && env.Type() == rxv1.TypeTimeSyncReply
	})
	_ = id

	attach := c.send("device.attach", map[string]any{"device_id": "vhf", "fft": map[string]any{"codec": "u8-db", "fps": 5}})
	c.until("attach ack", isAck(attach))

	var fftStream uint16

	c.until("stream.open", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		if env == nil || env.Type() != rxv1.TypeStreamOpen {
			return false
		}

		var p struct {
			StreamID uint16 `json:"stream_id"`
			Kind     string `json:"kind"`
		}

		_ = json.Unmarshal(env.Payload(), &p)
		fftStream = p.StreamID

		return p.Kind == "fft"
	})

	c.until("device running", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		if env == nil || env.Type() != rxv1.TypeDeviceState {
			return false
		}

		var p struct {
			State string `json:"state"`
		}

		_ = json.Unmarshal(env.Payload(), &p)

		return p.State == "running"
	})

	c.until("FFT frame", func(_ *rxv1.Envelope, h *rxv1.FrameHeader, p []byte) bool {
		return h != nil && h.Type == rxv1.FrameFFT && h.Codec == rxv1.CodecFFTU8DB && h.StreamID == fftStream && len(p) == 8+4096 && h.TimestampUS > 0
	})

	create := c.send("demod.create", map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 250_000 / 8})
	c.until("demod ack", isAck(create))

	frames := 0

	c.until("audio frames", func(_ *rxv1.Envelope, h *rxv1.FrameHeader, p []byte) bool {
		if h != nil && h.Type == rxv1.FrameAudio {
			if h.Codec != rxv1.CodecADPCMIMA || len(p) != 4+120 {
				t.Fatalf("audio frame %+v (%d bytes)", h, len(p))
			}

			frames++
		}

		return frames >= 10
	})

	retune := c.send("device.retune", map[string]any{"device_id": "vhf", "center_hz": 145_000_000})
	acked, patched := false, false

	c.until("retune ack and config patch", func(env *rxv1.Envelope, h *rxv1.FrameHeader, p []byte) bool {
		acked = acked || isAck(retune)(env, h, p)
		patched = patched || (env != nil && env.Type() == rxv1.TypeDeviceConfigPatch)

		return acked && patched
	})

	// The connector runs in its private workdir under node.runtime_dir,
	// the only place the node writes (GRID-002).
	if fi, err := os.Stat(filepath.Join(e.nodeCfg.Node.RuntimeDir, "sessions", "dev-vhf")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("connector workdir: %v", err)
	}

	// The hub mirrors the runtime state of the device.
	eventually(t, "hub device state", 10*time.Second, func() bool {
		d, err := e.g.devices.Get(context.Background(), "vhf")
		if err != nil {
			return false
		}

		st, _, _ := d.State()

		return st == domain.StateRunning && d.CenterFreq() != nil && *d.CenterFreq() == 145_000_000
	})
}

// TestNodeEnforcesListenPolicy: the node wiring hands the desired state to
// the media server (media.Options.Policy; nil would fail open), so a node
// refuses an anonymous token on a device its config marks registered,
// even when the token's scope names it (SRC-023).
func TestNodeEnforcesListenPolicy(t *testing.T) {
	e := newGridEnv(t, fastTimings())

	e.nodeCfg.Node.RuntimeDir = filepath.Join(t.TempDir(), "run")
	e.nodeCfg.Tools.RTLConnector = fakeConnector(t)
	e.nodeCfg.Devices = map[string]config.DeviceConfig{"vhf": {
		Name: "VHF", Type: "rtl_sdr", FreqRange: config.FreqRange{Min: config.MustFrequency("144MHz"), Max: config.MustFrequency("146MHz")},
		SampleRates: []int64{250_000}, ListenPolicy: "registered",
	}}

	e.enrollNode(t, fakeProber{})

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	now := time.Now()

	raw, err := e.g.keys.Issue(context.Background(), token.Claims{
		Issuer: e.hubCfg.Hub.URL, Audience: token.Audience("attic"), Subject: token.AnonymousSubject, ConnectionID: "anon-1",
		Roles: []string{}, IssuedAt: now.Add(-time.Second), NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
		Scopes: []token.Scope{{Device: "vhf", Perms: []string{token.PermListen, token.PermDemod}}}, Limits: token.Limits{MaxDemods: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	var ws *websocket.Conn

	eventually(t, "keys installed", 5*time.Second, func() bool {
		var st int
		ws, st = e.dialMedia(t, raw, "anon-1")

		return ws != nil && st == 101
	})

	c := &mediaClient{t: t, ws: ws}
	c.send("session.hello", map[string]any{"client": map[string]any{"name": "test", "version": "1"}})
	c.until("welcome", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		return env != nil && env.Type() == rxv1.TypeSessionWelcome
	})

	c.send("device.attach", map[string]any{"device_id": "vhf"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		if typ != websocket.MessageText {
			t.Fatal("a binary frame reached an anonymous visitor of a registered device")
		}

		env, err := rxv1.DecodeEnvelope(b)
		if err != nil {
			t.Fatal(err)
		}

		switch env.Type() {
		case rxv1.TypeError:
			if p := string(env.Payload()); !strings.Contains(p, `"forbidden"`) || !strings.Contains(p, "signed-in listener") {
				t.Fatalf("refusal = %s", p)
			}

			return
		case rxv1.TypeAck:
			t.Fatalf("anonymous attach accepted: %s", env.Payload())
		}
	}
}
