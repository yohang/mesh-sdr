package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	gridinfra "github.com/yohang/mesh-sdr/internal/grid/infra"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	idomain "github.com/yohang/mesh-sdr/internal/identity/domain"
)

var discard = slog.New(slog.DiscardHandler)

// roleAuthz authorizes callers holding role, like the identity module.
type roleAuthz struct{ role idomain.Role }

func (a roleAuthz) Authorize(_ context.Context, need idomain.Role) error {
	switch {
	case a.role >= need:
		return nil
	case a.role == idomain.RoleAnonymous:
		return idomain.ErrUnauthenticated
	default:
		return idomain.ErrForbidden
	}
}

var (
	anonymous = roleAuthz{idomain.RoleAnonymous}
	listener  = roleAuthz{idomain.RoleListener}
	admin     = roleAuthz{idomain.RoleAdmin}
)

type ca struct{}

func (ca) Fingerprint() (string, error) { return "AA:BB", nil }

func newServer(t *testing.T, auth api.Authorizer) *httptest.Server {
	t.Helper()

	a := dbtest.NewSQLite(t)
	nodes := gridapp.NewNodes(sqlite.NewNodeRepository(a), sqlite.NewRevocationRepository(a), a,
		gridinfra.NewLogAuditor(discard), ca{}, gridapp.DefaultTimings(), time.Now, discard)

	caps := gridapp.NewCapabilities(sqlite.NewCapabilityRepository(a), sqlite.NewNodeRepository(a), nil, discard)

	srv := httptest.NewServer(api.NewHandler(api.Server{
		GridHandlers: api.NewGridHandlers(auth, nodes, gridapp.NewHistory(), caps,
			gridapp.NewDevices(sqlite.NewDeviceRepository(a), gridinfra.NewLogAuditor(discard), discard),
			gridapp.NewPresence(sqlite.NewConnectionRepository(a), sqlite.NewDeviceRepository(a), nil, gridapp.DefaultTimings(), time.Now, discard)),
	}, auth, discard))
	t.Cleanup(srv.Close)

	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()

	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}

	req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)

	return resp.StatusCode, out
}

func TestNodesRequireAdmin(t *testing.T) {
	for _, tc := range []struct {
		auth   roleAuthz
		status int
		code   string
	}{{anonymous, http.StatusUnauthorized, "unauthenticated"}, {listener, http.StatusForbidden, "forbidden"}} {
		testNodesDenied(t, newServer(t, tc.auth), tc.status, tc.code)
	}
}

func testNodesDenied(t *testing.T, srv *httptest.Server, wantStatus int, wantCode string) {
	t.Helper()

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/nodes"},
		{http.MethodPost, "/nodes"},
		{http.MethodGet, "/nodes/attic"},
		{http.MethodPatch, "/nodes/attic"},
		{http.MethodDelete, "/nodes/attic"},
		{http.MethodPost, "/nodes/attic/enrollment-token"},
		{http.MethodPost, "/nodes/attic/revoke"},
		{http.MethodGet, "/nodes/attic/capabilities"},
		{http.MethodPost, "/nodes/attic/capabilities/probe"},
		{http.MethodGet, "/devices"},
		{http.MethodGet, "/devices/hf"},
	} {
		body := map[string]any{"id": "attic", "url": "https://x:1", "version": 1}

		status, out := call(t, srv, c.method, c.path, body)
		if status != wantStatus || out["code"] != wantCode {
			t.Errorf("%s %s = %d %v", c.method, c.path, status, out)
		}
	}
}

func TestNodesCRUD(t *testing.T) {
	srv := newServer(t, admin)

	status, out := call(t, srv, http.MethodPost, "/nodes", map[string]any{"id": "attic", "url": "https://10.0.0.1:8074"})
	if status != http.StatusCreated || out["enrollment_token"] == "" || out["ca_fingerprint"] != "AA:BB" {
		t.Fatalf("create = %d %v", status, out)
	}

	node, _ := out["node"].(map[string]any)
	if node["health"] != "enrolling" || node["name"] != "attic" {
		t.Errorf("node = %v", node)
	}

	if status, out := call(t, srv, http.MethodPost, "/nodes", map[string]any{"id": "attic", "url": "https://10.0.0.1:8074"}); status != http.StatusConflict || out["code"] != "node_exists" {
		t.Errorf("duplicate = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodPost, "/nodes", map[string]any{"id": "Bad", "url": "https://10.0.0.1:8074"}); status != http.StatusUnprocessableEntity || out["code"] != "invalid_node_id" {
		t.Errorf("invalid id = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodPatch, "/nodes/attic", map[string]any{"version": 9, "name": "x"}); status != http.StatusConflict || out["code"] != "version_conflict" {
		t.Errorf("stale patch = %d %v", status, out)
	}

	status, out = call(t, srv, http.MethodPatch, "/nodes/attic", map[string]any{"version": 2, "name": "Attic", "disabled": true})
	if status != http.StatusOK || out["name"] != "Attic" || out["disabled"] != true {
		t.Errorf("patch = %d %v", status, out)
	}

	status, out = call(t, srv, http.MethodGet, "/nodes/attic", nil)
	if status != http.StatusOK || out["load_history"] == nil {
		t.Errorf("get = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodGet, "/nodes/attic/capabilities", nil); status != http.StatusNotFound || out["code"] != "capabilities_not_reported" {
		t.Errorf("capabilities before report = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodPost, "/nodes/attic/capabilities/probe", nil); status != http.StatusServiceUnavailable || out["code"] != "node_unavailable" {
		t.Errorf("probe of an offline node = %d %v", status, out)
	}

	status, out = call(t, srv, http.MethodPost, "/nodes/attic/enrollment-token", nil)
	if status != http.StatusOK || out["enrollment_token"] == "" {
		t.Errorf("token = %d %v", status, out)
	}

	status, out = call(t, srv, http.MethodGet, "/nodes", nil)
	if items, _ := out["items"].([]any); status != http.StatusOK || len(items) != 1 {
		t.Errorf("list = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodGet, "/devices", nil); status != http.StatusOK || out["items"] == nil {
		t.Errorf("devices = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodGet, "/devices/hf", nil); status != http.StatusNotFound || out["code"] != "device_not_found" {
		t.Errorf("unknown device = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodPost, "/nodes/attic/revoke", nil); status != http.StatusOK || out["enrollment_state"] != "revoked" {
		t.Errorf("revoke = %d %v", status, out)
	}

	if status, out := call(t, srv, http.MethodPost, "/nodes/attic/revoke", nil); status != http.StatusConflict || out["code"] != "node_revoked" {
		t.Errorf("revoke twice = %d %v", status, out)
	}

	if status, _ := call(t, srv, http.MethodDelete, "/nodes/attic", nil); status != http.StatusNoContent {
		t.Errorf("delete = %d", status)
	}

	if status, out := call(t, srv, http.MethodGet, "/nodes/attic", nil); status != http.StatusNotFound || out["code"] != "node_not_found" {
		t.Errorf("get deleted = %d %v", status, out)
	}
}

func TestConnectionsCountIsPublic(t *testing.T) {
	for _, tc := range []struct {
		auth      roleAuthz
		wantItems bool
	}{{anonymous, false}, {listener, false}, {admin, true}} {
		srv := newServer(t, tc.auth)

		status, out := call(t, srv, http.MethodGet, "/connections", nil)
		_, hasItems := out["items"]

		if status != http.StatusOK || out["count"] != float64(0) || hasItems != tc.wantItems {
			t.Errorf("%T: %d %v", tc.auth, status, out)
		}
	}
}
