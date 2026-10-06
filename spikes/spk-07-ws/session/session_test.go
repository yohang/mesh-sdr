package session_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/session"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/transport"
)

var libs = []transport.Lib{transport.Coder{}, transport.Gorilla{}}

func startServer(t *testing.T, lib transport.Lib) string {
	t.Helper()
	srv := session.NewServer(lib, session.DefaultConfig())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(transport.SmallSendBufferListener{Listener: ln, SndBuf: 16 << 10}) }()
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Run(ctx)
	t.Cleanup(func() { cancel(); _ = hs.Close() })
	return "ws://" + ln.Addr().String() + "/ws"
}

func hello(t *testing.T) []byte {
	t.Helper()
	env, err := rxv1.NewEnvelope(rxv1.TypeSessionHello, rxv1.CorrelationID{}, time.Now().UnixMilli(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

// forEachPair runs f for every server × client library combination.
func forEachPair(t *testing.T, f func(t *testing.T, srv, cli transport.Lib)) {
	for _, s := range libs {
		for _, c := range libs {
			t.Run(s.Name()+"<-"+c.Name(), func(t *testing.T) {
				t.Parallel()
				f(t, s, c)
			})
		}
	}
}

func TestSubprotocolRequired426(t *testing.T) {
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		_, resp, err := cli.Dial(context.Background(), url, nil, 0)
		if err == nil {
			t.Fatal("dial without subprotocol must fail")
		}
		if resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
			t.Fatalf("resp = %v, want 426", resp)
		}
	})
}

func TestSubprotocolNegotiated(t *testing.T) {
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		c, resp, err := cli.Dial(context.Background(), url, []string{"rx.v2", rxv1.Subprotocol}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		if c.Subprotocol() != rxv1.Subprotocol || resp.Header.Get("Sec-WebSocket-Protocol") != rxv1.Subprotocol {
			t.Fatalf("negotiated %q / header %q", c.Subprotocol(), resp.Header.Get("Sec-WebSocket-Protocol"))
		}
		if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
			t.Fatalf("no extension (permessage-deflate) may be negotiated, got %q", ext)
		}
	})
}

func TestHandshakeAndStreams(t *testing.T) {
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := cli.Dial(ctx, url, []string{rxv1.Subprotocol}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		if err := c.WriteText(ctx, hello(t)); err != nil {
			t.Fatal(err)
		}
		var types []rxv1.MessageType
		var gotAudio, gotFFT bool
		for !gotAudio || !gotFFT {
			bin, msg, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !bin {
				env, err := rxv1.DecodeEnvelope(msg)
				if err != nil {
					t.Fatal(err)
				}
				types = append(types, env.Type())
				continue
			}
			h, _, err := rxv1.ParseFrame(msg)
			if err != nil {
				t.Fatal(err)
			}
			if len(types) < 3 {
				t.Fatalf("binary frame before stream.open: %v", types)
			}
			gotAudio = gotAudio || h.Type == rxv1.FrameAudio
			gotFFT = gotFFT || h.Type == rxv1.FrameFFT
		}
		if types[0] != rxv1.TypeSessionWelcome {
			t.Fatalf("first message %s", types[0])
		}
	})
}

func expectClose(t *testing.T, cli transport.Lib, c transport.Conn, want rxv1.CloseCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if got := cli.CloseCode(err); got != int(want) {
				t.Fatalf("close code = %d (%v), want %d", got, err, want)
			}
			return
		}
	}
}

func TestClientBinaryFrameCloses1003(t *testing.T) {
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		ctx := context.Background()
		c, _, err := cli.Dial(ctx, url, []string{rxv1.Subprotocol}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		_ = c.WriteText(ctx, hello(t))
		_ = c.WriteBinary(ctx, []byte{1, 2, 3})
		expectClose(t, cli, c, rxv1.CloseUnsupportedData)
	})
}

func TestOversizedTextCloses1009(t *testing.T) {
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		ctx := context.Background()
		c, _, err := cli.Dial(ctx, url, []string{rxv1.Subprotocol}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		_ = c.WriteText(ctx, hello(t))
		_ = c.WriteText(ctx, []byte(`{"pad":"`+strings.Repeat("x", rxv1.MaxInboundTextBytes)+`"}`))
		expectClose(t, cli, c, rxv1.CloseMessageTooBig)
	})
}

func TestHandshakeTimeout4408(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 5 s")
	}
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		c, _, err := cli.Dial(context.Background(), url, []string{rxv1.Subprotocol}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		expectClose(t, cli, c, rxv1.CloseHandshakeTimeout)
	})
}

func TestSlowConsumerClose4413(t *testing.T) {
	if testing.Short() {
		t.Skip("takes > 10 s")
	}
	forEachPair(t, func(t *testing.T, srv, cli transport.Lib) {
		url := startServer(t, srv)
		// 16 kB/s < audio bitrate: the audio backlog sits at its cap → 4413
		// after 10 s. The client keeps reading slowly, so the close frame
		// eventually gets through the TCP buffers.
		res := session.RunClient(context.Background(), cli, session.ClientConfig{
			URL: url, RateBps: 16_000, Duration: 40 * time.Second, RcvBuf: 16 << 10, AudioStep: 20 * time.Millisecond,
		})
		if res.CloseCode != int(rxv1.CloseSlowConsumer) {
			t.Fatalf("close code = %d (%v), want 4413", res.CloseCode, res.Err)
		}
		if res.Audio.GapEvents == 0 || res.Audio.FlaggedGaps != res.Audio.GapEvents {
			t.Fatalf("audio gaps %d, flagged %d: every gap must carry the discontinuity flag", res.Audio.GapEvents, res.Audio.FlaggedGaps)
		}
	})
}
