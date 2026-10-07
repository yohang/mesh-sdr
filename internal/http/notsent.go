package http

import (
	"context"
	"crypto/tls"
	"net"
)

// NotSentLowat returns an http.Server ConnContext that caps the unsent
// bytes the kernel buffers on every accepted TCP connection
// (TCP_NOTSENT_LOWAT, ADR 0004 decision 2): a slow media WebSocket reader
// then backs up into the node send queue, where the §6.8 drop policy
// applies, instead of into socket buffers. n ≤ 0 leaves the default.
func NotSentLowat(n int) func(ctx context.Context, c net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		if n <= 0 {
			return ctx
		}

		if tc, ok := c.(*tls.Conn); ok {
			c = tc.NetConn()
		}

		if tcp, ok := c.(*net.TCPConn); ok {
			if raw, err := tcp.SyscallConn(); err == nil {
				_ = raw.Control(func(fd uintptr) { setNotSentLowat(fd, n) })
			}
		}

		return ctx
	}
}
