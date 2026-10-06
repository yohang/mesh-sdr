package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestSetEnabled(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	root := e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	rv := &recRevocations{}
	acc := e.accounts(rv)
	by := e.actor(t, "root")

	in, _ := e.login("alice", password)

	if changed, err := acc.SetEnabled(ctx, by, u.ID(), false); err != nil || !changed {
		t.Fatalf("disable = %v, %v", changed, err)
	}

	if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Error("session survived")
	}

	if changed, _ := acc.SetEnabled(ctx, by, u.ID(), false); changed {
		t.Error("disable twice changed")
	}

	if changed, err := acc.SetEnabled(ctx, by, u.ID(), true); err != nil || !changed {
		t.Errorf("enable = %v, %v", changed, err)
	}

	if _, err := acc.SetEnabled(ctx, by, root.ID(), false); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("disable the last admin: %v", err)
	}

	if len(rv.got) != 1 {
		t.Errorf("revocations = %v", rv.got)
	}
}

func TestSetGeneratedPassword(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	acc := e.accounts(nil)

	in, _ := e.login("alice", password)

	pw, err := acc.SetGeneratedPassword(ctx, e.actor(t, "root"), u.ID())
	if err != nil || len(pw) < 20 {
		t.Fatalf("generated = %q, %v", pw, err)
	}

	if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Error("session survived")
	}

	res, err := e.login("alice", pw)
	if err != nil || !res.Principal.MustChangePassword() {
		t.Errorf("login with the generated password: %v", err)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	acc := e.accounts(nil)

	users, err := acc.Search(ctx, domain.UserQuery{Text: "example"})
	if err != nil || len(users) != 1 || users[0].Username().String() != "alice" {
		t.Errorf("search = %v, %v", users, err)
	}
}
