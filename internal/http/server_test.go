package http_test

import (
	"net/http"
	"testing"

	httpserver "github.com/yohang/mesh-sdr/internal/http"
)

func TestNewServerTimeouts(t *testing.T) {
	srv := httpserver.NewServer(":0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("timeouts = header %v, read %v, idle %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}

	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want unset (long-lived WebSocket responses)", srv.WriteTimeout)
	}
}
