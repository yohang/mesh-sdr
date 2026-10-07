package http_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// TestShellUser covers the user menu data (UI-010): nil for anonymous
// visitors, otherwise the name, the role badge and the account links.
func TestShellUser(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleOperator)

	c := h.client()

	read := func() string {
		t.Helper()

		res := c.do(http.MethodGet, "/shell-user", "", "", nil)
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}

		return string(b)
	}

	if got := read(); got != "anonymous" {
		t.Errorf("anonymous = %q", got)
	}

	c.session()

	if res := c.login("alice", password, false); res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", res.StatusCode)
	}

	if got, want := read(), "alice|Operator|Account=/account"; got != want {
		t.Errorf("signed in = %q, want %q", got, want)
	}
}
