package http

import (
	"net/http/httptest"
	"testing"
)

// TestBackground: live fragment refreshes and the events WebSocket upgrade
// do not count as activity of the session (ADR 0018).
func TestBackground(t *testing.T) {
	for _, tt := range []struct {
		path    string
		headers map[string]string
		want    bool
	}{
		{"/admin/nodes", nil, false},
		{"/admin/nodes", map[string]string{BackgroundHeader: "1"}, true},
		{"/api/ws", map[string]string{"Upgrade": "websocket"}, true},
		{"/api/ws", map[string]string{"Upgrade": "WebSocket"}, true},
		{"/api/ws", nil, false},
		{"/nodes/attic/ws", map[string]string{"Upgrade": "websocket"}, false},
	} {
		r := httptest.NewRequest("GET", tt.path, nil)
		for k, v := range tt.headers {
			r.Header.Set(k, v)
		}

		if got := background(r); got != tt.want {
			t.Errorf("%s %v: background = %v, want %v", tt.path, tt.headers, got, tt.want)
		}
	}
}
