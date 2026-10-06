// Package transport hides the WebSocket library behind a tiny interface so
// that the same rx.v1 session code runs on coder/websocket and
// gorilla/websocket.
package transport

import (
	"context"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// Conn is the subset of a WebSocket connection an rx.v1 session needs.
// Read is called from one goroutine, Write* from one other goroutine, and
// Close/CloseNow from any goroutine.
type Conn interface {
	Subprotocol() string
	Read(ctx context.Context) (binary bool, data []byte, err error)
	WriteText(ctx context.Context, b []byte) error
	WriteBinary(ctx context.Context, b []byte) error
	// Close sends a close frame with code (bounded wait) and releases the
	// connection.
	Close(code rxv1.CloseCode, reason string) error
	CloseNow() error
}

// Lib is one WebSocket library.
type Lib interface {
	Name() string
	// Accept upgrades the request, negotiating rx.v1 and disabling
	// compression. readLimit is the inbound message limit (1009 above it).
	Accept(w http.ResponseWriter, r *http.Request, readLimit int64) (Conn, error)
	// Dial opens a client connection offering rx.v1. rcvBuf > 0 shrinks
	// the client socket receive buffer (to make back-pressure visible fast).
	Dial(ctx context.Context, url string, subprotocols []string, rcvBuf int) (Conn, *http.Response, error)
	// CloseCode extracts the peer close code from a read error, or -1.
	CloseCode(err error) int
}

// OffersSubprotocol reports whether the upgrade request lists proto in
// Sec-WebSocket-Protocol. Both libraries happily complete the handshake with
// no subprotocol, so §6.1 (426 when absent) needs this pre-check.
func OffersSubprotocol(r *http.Request, proto string) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			if strings.TrimSpace(p) == proto {
				return true
			}
		}
	}
	return false
}

// RequireSubprotocol answers 426 when the client does not offer proto.
func RequireSubprotocol(w http.ResponseWriter, r *http.Request, proto string) bool {
	if OffersSubprotocol(r, proto) {
		return true
	}
	w.Header().Set("Sec-WebSocket-Protocol", proto)
	http.Error(w, "subprotocol "+proto+" required", http.StatusUpgradeRequired)
	return false
}

// SmallBufferDialer returns a dial func that shrinks SO_RCVBUF.
func SmallBufferDialer(rcvBuf int) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if rcvBuf > 0 {
		d.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBuf)
			})
			if err != nil {
				return err
			}
			return serr
		}
	}
	return d.DialContext
}

// SmallSendBufferListener shrinks SO_SNDBUF on every accepted connection.
type SmallSendBufferListener struct {
	net.Listener
	SndBuf int
}

// Accept implements net.Listener.
func (l SmallSendBufferListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok && l.SndBuf > 0 {
		_ = tc.SetWriteBuffer(l.SndBuf)
	}
	return c, nil
}
