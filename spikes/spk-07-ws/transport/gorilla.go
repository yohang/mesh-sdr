package transport

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// Gorilla is github.com/gorilla/websocket.
type Gorilla struct{}

// DefaultWriteTimeout bounds a gorilla write when ctx has no deadline.
const DefaultWriteTimeout = 30 * time.Second

var gorillaWritePool = &sync.Pool{}

// Name implements Lib.
func (Gorilla) Name() string { return "gorilla/websocket" }

// Accept implements Lib.
func (Gorilla) Accept(w http.ResponseWriter, r *http.Request, readLimit int64) (Conn, error) {
	up := websocket.Upgrader{
		Subprotocols:      []string{rxv1.Subprotocol},
		EnableCompression: false,
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		WriteBufferPool:   gorillaWritePool,
	}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(readLimit)
	return newGorillaConn(c), nil
}

// Dial implements Lib.
func (Gorilla) Dial(ctx context.Context, url string, subprotocols []string, rcvBuf int) (Conn, *http.Response, error) {
	d := websocket.Dialer{
		Subprotocols:     subprotocols,
		NetDialContext:   SmallBufferDialer(rcvBuf),
		HandshakeTimeout: 5 * time.Second,
	}
	c, resp, err := d.DialContext(ctx, url, nil)
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(rxv1.MaxFrameBytes)
	return newGorillaConn(c), resp, nil
}

// CloseCode implements Lib.
func (Gorilla) CloseCode(err error) int {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

type gorillaConn struct {
	c         *websocket.Conn
	readDone  chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newGorillaConn(c *websocket.Conn) *gorillaConn {
	return &gorillaConn{c: c, readDone: make(chan struct{})}
}

func (x *gorillaConn) Subprotocol() string { return x.c.Subprotocol() }

// Read emulates context support with read deadlines: gorilla has none.
func (x *gorillaConn) Read(ctx context.Context) (bool, []byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = x.c.SetReadDeadline(dl)
	} else {
		_ = x.c.SetReadDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = x.c.SetReadDeadline(time.Now()) })
	defer stop()
	typ, b, err := x.c.ReadMessage()
	if err != nil {
		x.readOnce.Do(func() { close(x.readDone) })
		return false, nil, err
	}
	return typ == websocket.BinaryMessage, b, nil
}

func (x *gorillaConn) write(ctx context.Context, typ int, b []byte) error {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(DefaultWriteTimeout)
	}
	_ = x.c.SetWriteDeadline(dl)
	return x.c.WriteMessage(typ, b)
}

func (x *gorillaConn) WriteText(ctx context.Context, b []byte) error {
	return x.write(ctx, websocket.TextMessage, b)
}

func (x *gorillaConn) WriteBinary(ctx context.Context, b []byte) error {
	return x.write(ctx, websocket.BinaryMessage, b)
}

// Close performs the close handshake by hand (gorilla leaves it to the
// application): send the close frame, wait up to 2 s for the reader to see
// the peer's close, then drop the TCP connection.
func (x *gorillaConn) Close(code rxv1.CloseCode, reason string) error {
	var err error
	x.closeOnce.Do(func() {
		err = x.c.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(int(code), reason), time.Now().Add(2*time.Second))
		select {
		case <-x.readDone:
		case <-time.After(2 * time.Second):
		}
		_ = x.c.Close()
	})
	return err
}

func (x *gorillaConn) CloseNow() error { return x.c.Close() }
