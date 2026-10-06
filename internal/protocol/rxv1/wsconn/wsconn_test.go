package wsconn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

func wsURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

func TestAcceptRequiresSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = wsconn.Accept(w, r, rxv1.ControlSubprotocol)
	}))
	defer srv.Close()

	_, resp, err := websocket.Dial(context.Background(), wsURL(srv), nil)
	if err == nil {
		t.Fatal("dial without subprotocol succeeded")
	}

	if resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("response = %v, want 426", resp)
	}

	if _, err := wsconn.Dial(context.Background(), wsURL(srv), nil, rxv1.Subprotocol); err == nil {
		t.Fatal("dial with another subprotocol succeeded")
	} else {
		var de *wsconn.DialError
		if !errors.As(err, &de) || de.Status != http.StatusUpgradeRequired {
			t.Fatalf("err = %v, want DialError 426", err)
		}
	}
}

// echo serves an rx-ctl.v1 endpoint that echoes envelopes and answers
// malformed frames with an error frame.
func echo(t *testing.T, opts wsconn.Options) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsconn.Accept(w, r, rxv1.ControlSubprotocol)
		if err != nil {
			return
		}

		c := wsconn.New(r.Context(), ws, opts)

		for {
			env, err := c.Read(r.Context())

			var pe *rxv1.Error
			if errors.As(err, &pe) {
				e, _ := rxv1.NewErrorEnvelope(0, rxv1.ErrorPayloadFrom(err, ""))
				_ = c.Send(e)

				continue
			}

			if err != nil {
				return
			}

			if err := c.Send(env); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestRoundTrip(t *testing.T) {
	srv := echo(t, wsconn.Options{ReadLimit: rxv1.MaxInboundControlTextBytes})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, err := wsconn.Dial(ctx, wsURL(srv), nil, rxv1.ControlSubprotocol)
	if err != nil {
		t.Fatal(err)
	}

	c := wsconn.New(ctx, ws, wsconn.Options{PingInterval: 10 * time.Millisecond, PongTimeout: time.Second})
	defer c.Close(rxv1.CloseNormal, "")

	env, _ := rxv1.NewEnvelope(rxv1.TypeCtlPing, rxv1.MustCorrelationID("p-1"), 1, nil)
	if err := c.Send(env); err != nil {
		t.Fatal(err)
	}

	got, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if got.Type() != rxv1.TypeCtlPing {
		t.Fatalf("type = %s", got.Type())
	}

	// A malformed frame is answered with an error frame; the channel stays up.
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}

	got, err = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if got.Type() != rxv1.TypeError {
		t.Fatalf("type = %s, want error", got.Type())
	}

	// Pings flowed meanwhile and the connection is still open.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-c.Done():
		t.Fatalf("connection closed: %v", c.Err())
	default:
	}
}

func TestSlowConsumerClosesWith4413(t *testing.T) {
	sent := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsconn.Accept(w, r, rxv1.ControlSubprotocol)
		if err != nil {
			return
		}

		c := wsconn.New(r.Context(), ws, wsconn.Options{MaxQueueBytes: 64})
		env, _ := rxv1.NewEnvelope(rxv1.TypeCtlHello, rxv1.CorrelationID{}, 1, map[string]string{"pad": strings.Repeat("x", 100)})
		sent <- c.Send(env)
		<-c.Done()
		_, _ = c.Read(r.Context())
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, err := wsconn.Dial(ctx, wsURL(srv), nil, rxv1.ControlSubprotocol)
	if err != nil {
		t.Fatal(err)
	}

	if err := <-sent; !errors.Is(err, wsconn.ErrSlowConsumer) {
		t.Fatalf("send = %v, want ErrSlowConsumer", err)
	}

	_, _, err = ws.Read(ctx)
	if got := wsconn.CloseStatus(err); got != rxv1.CloseSlowConsumer {
		t.Fatalf("close = %v (%v), want 4413", got, err)
	}
}

func TestBinaryFrameCloses1003(t *testing.T) {
	srv := echo(t, wsconn.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, err := wsconn.Dial(ctx, wsURL(srv), nil, rxv1.ControlSubprotocol)
	if err != nil {
		t.Fatal(err)
	}

	if err := ws.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	_, _, err = ws.Read(ctx)
	if got := wsconn.CloseStatus(err); got != rxv1.CloseUnsupportedData {
		t.Fatalf("close = %v (%v), want 1003", got, err)
	}
}
