package wire_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/wire"
)

var discard = slog.New(slog.DiscardHandler)

// serve runs p on a random local port and returns its base address.
func serve(t *testing.T, p *wire.Process) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	ln, err := p.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- p.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("graceful shutdown timed out")
		}
	})

	return ln.Addr().String()
}

func get(t *testing.T, c *http.Client, url string) (int, string, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func TestHub(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"

	addr := serve(t, wire.Hub(cfg, discard, dbtest.NewSQLite(t)))
	base := "http://" + addr

	tests := []struct {
		path   string
		status int
		ctype  string
	}{
		{"/api/v1/healthz/live", 200, "application/json"},
		{"/api/v1/healthz/ready", 200, "application/json"},
		{"/api/v1/openapi.json", 200, "application/json"},
		{"/api/v1/nope", 404, "application/problem+json"},
		{"/", 200, "text/html; charset=utf-8"},
		{"/nope", 404, "text/html; charset=utf-8"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			status, ctype, body := get(t, http.DefaultClient, base+tt.path)
			if status != tt.status || ctype != tt.ctype {
				t.Fatalf("got %d %q, want %d %q: %s", status, ctype, tt.status, tt.ctype, body)
			}

			if !json.Valid(body) && tt.ctype != "text/html; charset=utf-8" {
				t.Fatalf("invalid JSON: %s", body)
			}
		})
	}

	_, _, spec := get(t, http.DefaultClient, base+"/api/v1/openapi.json")

	var doc struct {
		OpenAPI string         `json:"openapi"`
		Paths   map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil || doc.OpenAPI == "" || doc.Paths["/healthz/ready"] == nil {
		t.Fatalf("openapi.json = %s (%v)", spec, err)
	}
}

type downDB struct{ db.Adapter }

func (downDB) Ping(context.Context) error { return errors.New("down") }

func TestHubNotReady(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"

	addr := serve(t, wire.Hub(cfg, discard, downDB{dbtest.NewSQLite(t)}))

	status, _, body := get(t, http.DefaultClient, "http://"+addr+"/api/v1/healthz/ready")
	if status != http.StatusServiceUnavailable || !json.Valid(body) {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestNode(t *testing.T) {
	cfg := config.DefaultNode()
	cfg.Node.ID = "attic"
	cfg.Node.Listen = "127.0.0.1:0"

	p, err := wire.Node(cfg, discard, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	addr := serve(t, p)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed pre-enrollment certificate
	}}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://"+addr+"/enroll", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /enroll = %d", resp.StatusCode)
	}

	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("TLS = %+v, want TLS 1.3", resp.TLS)
	}

	if uris := resp.TLS.PeerCertificates[0].URIs; len(uris) != 1 || uris[0].String() != "urn:rx:node:attic" {
		t.Errorf("peer URIs = %v", uris)
	}

	if status, _, _ := get(t, client, "https://"+addr+"/control"); status != http.StatusForbidden {
		t.Errorf("GET /control = %d, want 403", status)
	}
}

func TestOpenDB(t *testing.T) {
	ctx := context.Background()

	a, err := wire.OpenDB(ctx, config.DB{DSN: "sqlite://" + filepath.Join(t.TempDir(), "hub.db"), MaxReadConnections: 2}, discard)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if a.Dialect() != db.DialectSQLite {
		t.Errorf("dialect = %s", a.Dialect())
	}

	if _, err := wire.OpenDB(ctx, config.DB{DSN: "postgres://x/y"}, discard); !errors.Is(err, db.ErrEngineUnsupported) {
		t.Errorf("postgres: err = %v", err)
	}
}
