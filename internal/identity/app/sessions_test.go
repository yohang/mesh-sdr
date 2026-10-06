package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestDescribeUserAgent(t *testing.T) {
	for ua, want := range map[string][2]string{
		"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0":                                                    {"Firefox", "Linux"},
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36 Edg/129.0":     {"Edge", "Windows"},
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/604.1": {"Safari", "iOS"},
		"<script>": {"Unknown browser", "Unknown system"},
	} {
		if b, s := app.DescribeUserAgent(ua); b != want[0] || s != want[1] {
			t.Errorf("%q → %s / %s", ua, b, s)
		}
	}
}

func TestOwnSessions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	e.addUser(t, "bob", "", domain.RoleListener)
	acc := e.accounts(nil)

	other, _ := e.login("alice", password)
	me := e.actor(t, "alice")
	bob := e.actor(t, "bob")

	views, err := acc.OwnSessions(ctx, me)
	if err != nil || len(views) != 2 {
		t.Fatalf("sessions = %d, %v", len(views), err)
	}

	current := 0

	for _, v := range views {
		if v.Current {
			current++
		}

		if v.Ref == "" || v.IP != ip.String() {
			t.Errorf("view = %+v", v)
		}
	}

	if current != 1 {
		t.Errorf("%d current sessions", current)
	}

	// Another user's handle is not found (SR-18).
	if err := acc.RevokeOwnSession(ctx, bob, other.Session.Ref()); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("revoke another user's session: %v", err)
	}

	if n, err := acc.RevokeOtherSessions(ctx, me); err != nil || n != 1 {
		t.Errorf("revoke others = %d, %v", n, err)
	}

	if _, err := e.auth.Resolve(ctx, other.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Error("other session survived")
	}

	if _, err := acc.OwnSessions(ctx, app.Actor{}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("anonymous: %v", err)
	}
}

func TestAdminRevokesUserSessions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	rv := &recRevocations{}
	acc := e.accounts(rv)
	root := e.actor(t, "root")

	first, _ := e.login("alice", password)
	_, _ = e.login("alice", password)

	if err := acc.RevokeUserSession(ctx, root, u.ID(), first.Session.Ref()); err != nil {
		t.Fatal(err)
	}

	if err := acc.RevokeUserSession(ctx, root, u.ID(), first.Session.Ref()); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("revoke twice: %v", err)
	}

	if n, err := acc.RevokeUserSessions(ctx, root, u.ID()); err != nil || n != 1 {
		t.Errorf("revoke all = %d, %v", n, err)
	}

	if views, _ := acc.UserSessions(ctx, u.ID()); len(views) != 0 {
		t.Errorf("sessions left: %d", len(views))
	}

	if len(rv.got) != 2 || rv.got[0].Sessions[0] != first.Session.Ref() {
		t.Errorf("revocations = %+v", rv.got)
	}
}
