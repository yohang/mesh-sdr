package transport_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/transport"
)

// rawClient performs a minimal RFC 6455 client handshake on a plain TCP
// connection and then discards everything, so that the benchmark measures
// the server-side write path of each library only.
func rawClient(b *testing.B, addr string) net.Conn {
	b.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	req := "GET /ws HTTP/1.1\r\nHost: " + addr + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: rx.v1\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		b.Fatal(err)
	}
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			b.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(line, "HTTP/1.1 ") && !strings.HasPrefix(line, "HTTP/1.1 101") {
			b.Fatalf("handshake: %s", line)
		}
	}
	go func() { _, _ = io.Copy(io.Discard, br) }()
	return c
}

func BenchmarkServerWriteBinary(b *testing.B) {
	for _, lib := range []transport.Lib{transport.Coder{}, transport.Gorilla{}} {
		for _, size := range []int{508, 4128, 32 << 10} {
			for _, withTimeout := range []bool{true, false} {
				b.Run(fmt.Sprintf("%s/%dB/timeout=%v", lib.Name(), size, withTimeout), func(b *testing.B) {
					connCh := make(chan transport.Conn, 1)
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						b.Fatal(err)
					}
					hs := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						c, err := lib.Accept(w, r, rxv1.MaxInboundTextBytes)
						if err != nil {
							b.Error(err)
							return
						}
						connCh <- c
					})}
					go func() { _ = hs.Serve(ln) }()
					defer func() { _ = hs.Close() }()
					cli := rawClient(b, ln.Addr().String())
					defer func() { _ = cli.Close() }()
					c := <-connCh
					defer func() { _ = c.CloseNow() }()

					frame := make([]byte, size)
					ctx := context.Background()
					b.SetBytes(int64(size))
					b.ReportAllocs()
					for b.Loop() {
						if withTimeout {
							wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
							if err := c.WriteBinary(wctx, frame); err != nil {
								b.Fatal(err)
							}
							cancel()
						} else if err := c.WriteBinary(ctx, frame); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
