package wire

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
)

// form posts an HTML form as b (boosted like the app shell).
func (b *hubBrowser) form(path string, values url.Values) (int, string, http.Header) {
	b.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, b.base+path, strings.NewReader(values.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", b.csrf)
	req.Header.Set("Origin", b.base)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Boosted", "true")

	c := *b.c
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(out), resp.Header
}

func (b *hubBrowser) page(path string) (int, string) {
	b.t.Helper()

	st, body := b.do(http.MethodGet, path, "")

	return st, string(body)
}

var (
	tokenPattern   = regexp.MustCompile(`select-all">([A-Za-z0-9_-]{43})<`)
	versionPattern = regexp.MustCompile(`name="version" value="(\d+)"`)
)

// TestAdminNodesPages walks Admin › Nodes as an admin (add, enroll, edit,
// disable, probe, re-enroll, revoke, remove) and as an operator (read only)
// with a real node (GRID-005, GRID-009, GRID-015).
func TestAdminNodesPages(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings())
	admin := identity.UserAdmin(IdentityDeps(e.hubCfg, quiet, e.adapter))

	for name, role := range map[string]identitydomain.Role{
		"root": identitydomain.RoleAdmin, "op": identitydomain.RoleOperator, "lis": identitydomain.RoleListener,
	} {
		if _, err := admin.Add(context.Background(), identityapp.AddUserInput{Username: name, Role: role, Password: mediaPassword}); err != nil {
			t.Fatal(err)
		}
	}

	root, op, lis := e.signIn(t, "root"), e.signIn(t, "op"), e.signIn(t, "lis")

	if st, _ := lis.page("/admin/nodes"); st != http.StatusForbidden {
		t.Errorf("listener list = %d", st)
	}

	if st, _ := op.page("/admin/nodes/new"); st != http.StatusForbidden {
		t.Errorf("operator add form = %d", st)
	}

	if st, body := root.page("/admin/nodes/new"); st != http.StatusOK || !strings.Contains(body, `action="/admin/nodes"`) {
		t.Fatalf("add form = %d", st)
	}

	// A bad address keeps the form with the error.
	st, body, _ := root.form("/admin/nodes", url.Values{"id": {"attic"}, "url": {"ftp://x"}})
	if st != http.StatusUnprocessableEntity || !strings.Contains(body, "role=\"alert\"") || !strings.Contains(body, `value="attic"`) {
		t.Fatalf("add with a bad url = %d %s", st, body)
	}

	st, body, _ = root.form("/admin/nodes", url.Values{"id": {"attic"}, "name": {"Attic"}, "url": {"https://" + e.nodeAddr}})
	if st != http.StatusOK || !strings.Contains(body, "Node added") || !strings.Contains(body, "meshsdr node enroll --token-file") {
		t.Fatalf("add = %d %s", st, body)
	}

	m := tokenPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token on the page: %s", body)
	}

	tok, err := domain.ParseEnrollmentToken(m[1])
	if err != nil {
		t.Fatal(err)
	}

	// Shown once: the node page never shows it.
	if _, page := root.page("/admin/nodes/attic"); strings.Contains(page, m[1]) || !strings.Contains(page, "enrolling") {
		t.Fatal("the node page shows the enrollment token, or the node is not enrolling")
	}

	e.enrollWith(t, tok, pki.FormatFingerprint(e.ca.Fingerprint()), fakeProber{})

	eventually(t, "node online", 15*time.Second, func() bool {
		_, page := op.page("/admin/nodes")

		return strings.Contains(page, "online")
	})

	// The detail: health, devices, capabilities and the load graph, read
	// only for an operator.
	eventually(t, "load history", 10*time.Second, func() bool {
		_, page := op.page("/admin/nodes/attic")

		return strings.Contains(page, "<polyline")
	})

	_, page := op.page("/admin/nodes/attic")
	for _, want := range []string{`data-msdr-topics="nodes devices"`, `href="/admin/devices/hf"`, ">ready</span>", "Listen policy", "global listen policy", "Capabilities", "Load figures", "Fingerprint"} {
		if !strings.Contains(page, want) {
			t.Errorf("operator detail lacks %q", want)
		}
	}

	// The list shows each node's devices and their status.
	_, list := op.page("/admin/nodes")
	for _, want := range []string{`data-msdr-topics="nodes devices"`, `href="/admin/devices/hf"`, ">ready</span>"} {
		if !strings.Contains(list, want) {
			t.Errorf("node list lacks %q", want)
		}
	}

	if strings.Contains(page, "Manage") {
		t.Error("operator detail shows the admin forms")
	}

	if st, _, _ := op.form("/admin/nodes/attic/disable", nil); st != http.StatusForbidden {
		t.Errorf("operator disable = %d", st)
	}

	// The live fragment is the status part alone.
	st, frag := op.fragment("/admin/nodes/attic")
	if st != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(frag), `<div id="node-live"`) {
		t.Errorf("fragment = %d %.80s", st, frag)
	}

	version := func() string {
		_, page := root.page("/admin/nodes/attic")

		v := versionPattern.FindStringSubmatch(page)
		if v == nil {
			t.Fatal("no version on the edit form")
		}

		return v[1]
	}

	stale := version()

	st, body, _ = root.form("/admin/nodes/attic", url.Values{"name": {"Attic two"}, "url": {"https://" + e.nodeAddr}, "version": {stale}})
	if st != http.StatusOK || !strings.Contains(body, "The node is saved.") || !strings.Contains(body, "Attic two") {
		t.Fatalf("edit = %d", st)
	}

	if st, body, _ = root.form("/admin/nodes/attic", url.Values{"name": {"Attic three"}, "url": {"https://" + e.nodeAddr}, "version": {stale}}); st != http.StatusConflict {
		t.Errorf("edit at a stale version = %d %s", st, body)
	}

	if st, body, _ = root.form("/admin/nodes/attic/disable", nil); st != http.StatusOK || !strings.Contains(body, "The node is disabled") {
		t.Errorf("disable = %d", st)
	}

	if st, body, _ = root.form("/admin/nodes/attic/enable", nil); st != http.StatusOK || !strings.Contains(body, "The node is enabled") {
		t.Errorf("enable = %d", st)
	}

	eventually(t, "control channel back", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	if st, body, _ = root.form("/admin/nodes/attic/probe", nil); st != http.StatusOK || !strings.Contains(body, "asked to report its capabilities") {
		t.Errorf("probe = %d %s", st, body)
	}

	if st, body, _ = root.form("/admin/nodes/attic/revoke", url.Values{"confirm": {"1"}}); st != http.StatusOK || !strings.Contains(body, "revoked") {
		t.Errorf("revoke = %d", st)
	}

	if st, body, _ = root.form("/admin/nodes/attic/revoke", url.Values{"confirm": {"1"}}); st != http.StatusConflict {
		t.Errorf("revoke twice = %d %s", st, body)
	}

	if st, body, _ = root.form("/admin/nodes/attic/token", url.Values{"confirm": {"1"}}); st != http.StatusOK ||
		!strings.Contains(body, "New enrollment token") || tokenPattern.FindStringSubmatch(body) == nil {
		t.Errorf("new token = %d", st)
	}

	st, _, h := root.form("/admin/nodes/attic/delete", url.Values{"confirm": {"1"}})
	if st != http.StatusNoContent || h.Get("HX-Redirect") != "/admin/nodes" {
		t.Fatalf("delete = %d %v", st, h)
	}

	if st, _ := root.page("/admin/nodes/attic"); st != http.StatusNotFound {
		t.Errorf("deleted node = %d", st)
	}

	// Admin › Connections and the heartbeat settings are for admins.
	if st, page := root.page("/admin/connections"); st != http.StatusOK || !strings.Contains(page, "receivers") {
		t.Errorf("connections = %d", st)
	}

	if st, _ := op.page("/admin/connections"); st != http.StatusForbidden {
		t.Errorf("operator connections = %d", st)
	}

	if st, page := root.page("/admin/grid"); st != http.StatusOK || !strings.Contains(page, "grid.heartbeat_interval_s") {
		t.Errorf("heartbeat settings = %d", st)
	} else if !strings.Contains(page, `aria-current="page">Node health</a>`) {
		t.Error("Node health is not the current admin section")
	}

	if n := countRows(t, e, "SELECT count(*) FROM audit_log WHERE target_id = 'attic' AND action IN ('node.add', 'node.update', 'node.revoke', 'node.enrollment_token.issue', 'node.delete')"); n < 6 {
		t.Errorf("audit rows = %d", n)
	}
}

// fragment fetches the htmx fragment of a page (a live refresh).
func (b *hubBrowser) fragment(path string) (int, string) {
	b.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, b.base+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}

	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Msdr-Background", "1")

	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(out)
}

func countRows(t *testing.T, e *gridEnv, query string) int {
	t.Helper()

	var n int
	if err := e.adapter.Reader(context.Background()).QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}
