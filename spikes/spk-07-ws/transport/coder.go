package transport

import (
	"context"
	"net/http"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// Coder is github.com/coder/websocket.
type Coder struct{}

// Name implements Lib.
func (Coder) Name() string { return "coder/websocket" }

// Accept implements Lib.
func (Coder) Accept(w http.ResponseWriter, r *http.Request, readLimit int64) (Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{rxv1.Subprotocol},
		CompressionMode: websocket.CompressionDisabled, // §6.1: never on the media WS
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(readLimit)
	return coderConn{c}, nil
}

// Dial implements Lib.
func (Coder) Dial(ctx context.Context, url string, subprotocols []string, rcvBuf int) (Conn, *http.Response, error) {
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: subprotocols,
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: SmallBufferDialer(rcvBuf),
		}},
	})
	if err != nil {
		return nil, resp, err
	}
	// Default client read limit is 32 KiB; binary frames go up to 64 KiB.
	c.SetReadLimit(rxv1.MaxFrameBytes)
	return coderConn{c}, resp, nil
}

// CloseCode implements Lib.
func (Coder) CloseCode(err error) int { return int(websocket.CloseStatus(err)) }

type coderConn struct{ c *websocket.Conn }

func (x coderConn) Subprotocol() string { return x.c.Subprotocol() }

func (x coderConn) Read(ctx context.Context) (bool, []byte, error) {
	typ, b, err := x.c.Read(ctx)
	return typ == websocket.MessageBinary, b, err
}

func (x coderConn) WriteText(ctx context.Context, b []byte) error {
	return x.c.Write(ctx, websocket.MessageText, b)
}

func (x coderConn) WriteBinary(ctx context.Context, b []byte) error {
	return x.c.Write(ctx, websocket.MessageBinary, b)
}

func (x coderConn) Close(code rxv1.CloseCode, reason string) error {
	return x.c.Close(websocket.StatusCode(code), reason)
}

func (x coderConn) CloseNow() error { return x.c.CloseNow() }
