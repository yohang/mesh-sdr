package wire

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// page fetches an HTML page with the client's session.
func (c *apiClient) page(path string) string {
	c.h.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.h.url+path, nil)
	if err != nil {
		c.h.t.Fatal(err)
	}

	req.Header.Set("X-Forwarded-For", c.addr)

	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.h.t.Fatal(err)
	}

	return string(b)
}

// TestShellNavigationByRole covers UI-006 and UI-010 in the hub: the Admin
// section only for admins from admin.allowed_networks, the user menu for
// signed-in users.
func TestShellNavigationByRole(t *testing.T) {
	v, err := apitest.New(api.Spec())
	if err != nil {
		t.Fatal(err)
	}

	h := newContractHub(t, v, map[string]identitydomain.Role{"listener": identitydomain.RoleListener, "admin": identitydomain.RoleAdmin})

	// Visitors open Files when some device is anonymous-listenable.
	ctx, now := context.Background(), time.Now()
	if err := gridsqlite.NewNodeRepository(h.adapter).Create(ctx,
		domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("attic"), domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	hf, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{1},
		ListenPolicy: domain.ListenAnonymous,
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, hf); err != nil {
		t.Fatal(err)
	}

	const adminLink = `<a href="/admin" data-section="admin"`

	tests := []struct {
		name      string
		client    *apiClient
		admin     bool
		user      string
		signInBtn bool
	}{
		{"anonymous", h.client(10), false, "", true},
		{"listener", h.signedIn("listener", 10), false, `<span class="badge">Listener</span>`, false},
		{"admin", h.signedIn("admin", 10), true, `<span class="badge">Admin</span>`, false},
		{"admin outside admin networks", h.signedIn("admin", 192), false, `<span class="badge">Admin</span>`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.client.page("/files")

			if got := strings.Contains(body, adminLink); got != tt.admin {
				t.Errorf("admin link = %v, want %v", got, tt.admin)
			}

			if !strings.Contains(body, `<a href="/files" data-section="files" class="nav-link" aria-current="page"`) {
				t.Error("Files is not the current section")
			}

			if got := strings.Contains(body, `<a href="/login" class="font-medium">Sign in</a>`); got != tt.signInBtn {
				t.Errorf("sign in link = %v, want %v", got, tt.signInBtn)
			}

			if tt.user != "" && !strings.Contains(body, tt.user) {
				t.Errorf("user menu lacks %s", tt.user)
			}
		})
	}
}
