package wire

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// TestZVEIEndToEnd: a ZVEI sequence on the NFM carrier of the fake
// connector is decoded by multimon-ng on the node (DEC-035): the listener
// gets the decode over rx.v1, and the hub stores it in decoded_messages
// once (DEC-047).
func TestZVEIEndToEnd(t *testing.T) {
	if _, err := process.ResolveTool("multimon-ng", []string{"/usr/local/bin", "/usr/bin"}); err != nil {
		t.Skip("multimon-ng not installed")
	}

	e := newGridEnv(t, fastTimings())

	e.nodeCfg.Node.RuntimeDir = filepath.Join(t.TempDir(), "run")
	e.nodeCfg.Tools.RTLConnector = fakeConnector(t)
	e.nodeCfg.Devices = map[string]config.DeviceConfig{"vhf": {
		Name: "VHF", Type: "rtl_sdr", FreqRange: config.FreqRange{Min: config.MustFrequency("144MHz"), Max: config.MustFrequency("146MHz")},
		SampleRates: []int64{250_000}, ListenPolicy: "anonymous", Driver: config.Driver{Device: "zvei"},
	}}

	e.enrollNode(t, fakeProber{}, WithDecoderProbe())

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	now := time.Now()

	raw, err := e.g.keys.Issue(context.Background(), token.Claims{
		Issuer: e.hubCfg.Hub.URL, Audience: token.Audience("attic"), Subject: "u1", SessionID: token.SessionRef("s1"),
		ConnectionID: "m1", Roles: []string{"listener"}, IssuedAt: now.Add(-time.Second), NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
		Scopes: []token.Scope{{Device: "vhf", Perms: []string{token.PermListen, token.PermDemod}}},
		Limits: token.Limits{MaxDemods: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	var ws *websocket.Conn

	eventually(t, "keys installed", 5*time.Second, func() bool {
		var st int
		ws, st = e.dialMedia(t, raw, "m1")

		return ws != nil && st == 101
	})

	ws.SetReadLimit(1 << 20)

	c := &mediaClient{t: t, ws: ws}
	c.send("session.hello", map[string]any{"client": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{
		"audio_codecs": []string{"adpcm-ima"}, "fft_codecs": []string{"u8-db"}, "audio_rates": []int{12000}, "max_fft_fps": 1,
	}})
	c.until("welcome", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		return env != nil && env.Type() == rxv1.TypeSessionWelcome
	})

	c.until("attach ack", isAck(c.send("device.attach", map[string]any{"device_id": "vhf", "fft": map[string]any{"codec": "u8-db", "fps": 1}})))
	c.until("demod ack", isAck(c.send("demod.create", map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 250_000 / 8})))

	var demod string

	set := c.send("decoder.set", map[string]any{"demod_id": "d1", "decoder": "zvei"})
	c.until("decoder.set ack", func(env *rxv1.Envelope, h *rxv1.FrameHeader, p []byte) bool {
		if !isAck(set)(env, h, p) {
			return false
		}

		demod = "d1"

		return true
	})

	var text string

	c.until("decode", func(env *rxv1.Envelope, _ *rxv1.FrameHeader, _ []byte) bool {
		if env == nil || env.Type() != rxv1.TypeDecode {
			return false
		}

		var p struct {
			DemodID string `json:"demod_id"`
			Text    string `json:"text"`
		}

		_ = json.Unmarshal(env.Payload(), &p)
		text = p.Text

		return p.DemodID == demod && strings.HasPrefix(p.Text, "[ZVEI1]")
	})

	if text != "[ZVEI1] 12345" {
		t.Errorf("decode %q", text)
	}

	eventually(t, "decoded_messages row", 10*time.Second, func() bool {
		var n int

		err := e.adapter.Reader(context.Background()).QueryRowContext(context.Background(),
			`SELECT count(*) FROM decoded_messages WHERE device_id = 'vhf' AND mode = 'zvei' AND text = '[ZVEI1] 12345' AND node_id = 'attic'`).Scan(&n)

		return err == nil && n >= 1
	})
}
