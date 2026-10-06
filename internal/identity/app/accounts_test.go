package app_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
)

// recRevocations records published revocations.
type recRevocations struct{ got []app.Revocation }

func (r *recRevocations) PublishRevocation(_ context.Context, rv app.Revocation) {
	r.got = append(r.got, rv)
}

func (e *env) accounts(rv app.RevocationPublisher) *app.Accounts {
	return app.NewAccounts(app.AccountsDeps{
		Hasher: e.hasher, Policies: app.NewPolicies(nil, nil), Invitations: sqlite.NewInvitations(e.db), Passwords: e.passwords(),
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: e.db, Now: e.clock.Now, Revocations: rv,
		Logger: slog.New(slog.DiscardHandler),
	})
}

func (e *env) actor(t *testing.T, name string) app.Actor {
	t.Helper()

	in, err := e.login(name, password)
	if err != nil {
		t.Fatal(err)
	}

	return app.Actor{Principal: in.Principal, Meta: app.RequestMeta{IP: ip, RequestID: "req"}}
}

func grant(t *testing.T, r domain.Role, device string) domain.RoleGrant {
	t.Helper()

	var dev domain.DeviceID
	if device != "" {
		dev, _ = domain.NewDeviceID(device)
	}

	g, err := domain.NewRoleGrant(r, dev)
	if err != nil {
		t.Fatal(err)
	}

	return g
}

func TestSetRolesRevokesSessionsOnAnyChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "boss", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	boss := e.actor(t, "boss")
	rv := &recRevocations{}
	acc := e.accounts(rv)

	for i, grants := range [][]domain.RoleGrant{
		{grant(t, domain.RoleOperator, "rtl-1")}, // upgrade
		{grant(t, domain.RoleOperator, "")},      // upgrade
		nil,                                      // downgrade
	} {
		in, _ := e.login("alice", password)

		res, err := acc.SetRoles(ctx, boss, u.ID(), grants)
		if err != nil || !res.Changed || res.RevokedSessions < 1 {
			t.Fatalf("change %d: %+v, %v", i, res, err)
		}

		if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("change %d: session survived", i)
		}
	}

	if len(rv.got) != 3 || rv.got[0].Users[0] != u.ID() {
		t.Errorf("revocations = %+v", rv.got)
	}

	// No change: nothing revoked or published.
	in, _ := e.login("alice", password)

	if res, err := acc.SetRoles(ctx, boss, u.ID(), nil); err != nil || res.Changed {
		t.Errorf("no-op = %+v, %v", res, err)
	}

	if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); err != nil {
		t.Errorf("session revoked by a no-op: %v", err)
	}

	if got := e.actions(t); !slices.Contains(got, domain.ActionUserRoleUpdate+":") {
		t.Errorf("audit = %v", got)
	}

	if _, err := acc.SetRoles(ctx, boss, domain.UserID{}, nil); !errors.Is(err, domain.ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestTheLastAdminStays(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	root := e.addUser(t, "root", "", domain.RoleAdmin)
	acc := e.accounts(nil)
	self := e.actor(t, "root")

	if _, err := acc.SetRoles(ctx, self, root.ID(), nil); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("demote the last admin: %v", err)
	}

	if _, err := e.admin.Disable(ctx, "root"); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("disable the last admin: %v", err)
	}

	e.addUser(t, "second", "", domain.RoleAdmin)

	if _, err := acc.SetRoles(ctx, self, root.ID(), []domain.RoleGrant{grant(t, domain.RoleOperator, "")}); err != nil {
		t.Errorf("demote with another admin: %v", err)
	}

	if _, err := e.admin.Disable(ctx, "second"); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("disable the remaining admin: %v", err)
	}
}
