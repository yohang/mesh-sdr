package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/fakenode"
)

// browser simulates the browser side: TLS to the gateway, cookie, rx.v1.
type browser struct {
	host string // SNI / Host
	addr string // 127.0.0.1:port
	pool *x509.CertPool
}

func (b browser) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: b.pool, ServerName: b.host, NextProtos: []string{"http/1.1"}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, b.addr)
		},
		DisableKeepAlives: true,
	}, Timeout: 5 * time.Second}
}

type dialOpts struct {
	noCookie bool
	forged   bool
}

// dial opens /nodes/<id>/ws through the gateway. status is the HTTP status
// when the upgrade is refused (0 on success).
func (b browser) dial(ctx context.Context, nodeID string, o dialOpts) (*websocket.Conn, fakenode.Hello, int, error) {
	h := http.Header{}
	if !o.noCookie {
		h.Set("Cookie", "session=valid")
	}
	if o.forged {
		h.Set("X-Rx-Access-Token", "forged")
		h.Set("X-Rx-Node-Id", "evil")
	}
	return wsDial(ctx, "wss://"+b.host+"/nodes/"+nodeID+"/ws", b.httpClient(), h)
}

func wsDial(ctx context.Context, url string, hc *http.Client, h http.Header) (*websocket.Conn, fakenode.Hello, int, error) {
	var hello fakenode.Hello
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h, Subprotocols: []string{"rx.v1"}})
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		return nil, hello, st, err
	}
	_, msg, err := c.Read(ctx)
	if err != nil {
		return nil, hello, 0, err
	}
	if err := json.Unmarshal(msg, &hello); err != nil {
		return nil, hello, 0, err
	}
	return c, hello, 0, nil
}

func echo(ctx context.Context, c *websocket.Conn, payload string) error {
	if err := c.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		return err
	}
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if string(b) != payload {
		return fmt.Errorf("echo mismatch %q", b)
	}
	return nil
}

// pinger echoes every 50 ms and records when the connection died.
type pinger struct {
	diedAt atomic.Int64 // unix nanos, 0 = alive
	err    atomic.Value
}

func startPinger(c *websocket.Conn) *pinger {
	p := &pinger{}
	go func() {
		for i := 0; ; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := echo(ctx, c, fmt.Sprintf("p%d", i))
			cancel()
			if err != nil {
				p.diedAt.Store(time.Now().UnixNano())
				p.err.Store(err.Error())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return p
}

func (p *pinger) alive() bool { return p.diedAt.Load() == 0 }

func (p *pinger) diedAfter(t0 time.Time) time.Duration {
	d := p.diedAt.Load()
	if d == 0 {
		return -1
	}
	return time.Unix(0, d).Sub(t0)
}

