package app_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/memory"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settings"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func (e *env) invitations(n app.Notifier) *app.Invitations {
	return app.NewInvitations(app.InvitationsDeps{
		Invitations: sqlite.NewInvitations(e.db), Users: e.users, Audit: e.audit, Tx: e.db, Hasher: e.hasher,
		IDs: shared.NewUUIDv7Generator(), Now: e.clock.Now, Settings: settings.Defaults{}, Policies: app.NewPolicies(nil, nil),
		Auth: e.auth, Notifier: n, Links: app.NewLinks("https://hub.example"),
		Limiter: memory.NewIPLimiter(time.Millisecond, 1000, 100), Logger: slog.New(slog.DiscardHandler),
	})
}

func tokenOf(t *testing.T, link string) string {
	t.Helper()

	tok, ok := strings.CutPrefix(link, "https://hub.example/invite/")
	if !ok {
		t.Fatalf("link = %q", link)
	}

	return tok
}

func TestInvitationLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	root := e.actor(t, "root")
	n := &recNotifier{}
	inv := e.invitations(n)
	meta := app.RequestMeta{IP: ip}

	created, err := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleOperator, Device: "rtl-1", Email: "new@example.org"})
	if err != nil {
		t.Fatal(err)
	}

	if !created.Mailed || created.Invitation.Delivery() != domain.DeliveryEmail ||
		!created.Invitation.ExpiresAt().Equal(t0.Add(7*24*time.Hour)) {
		t.Errorf("created = %+v", created)
	}

	if got := n.all(); len(got) != 1 || got[0].kind != "invitation" || got[0].link != created.Link {
		t.Errorf("notifications = %v", got)
	}

	token := tokenOf(t, created.Link)

	if got, err := inv.Check(ctx, token, meta); err != nil || got.ID() != created.Invitation.ID() {
		t.Fatalf("check: %v", err)
	}

	res, err := inv.Accept(ctx, app.AcceptInput{Token: token, Username: "newbie", Email: "ignored@example.org", Password: fresh, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}

	if !res.Principal.HasOnDevice(domain.RoleOperator, mustDevice(t, "rtl-1")) || res.Principal.Has(domain.RoleOperator) {
		t.Errorf("grants = %v", res.Principal.Grants())
	}

	u, _ := e.users.ByID(ctx, res.Principal.UserID())
	if u.Email().String() != "new@example.org" || u.EmailVerifiedAt().IsZero() {
		t.Errorf("e-mail = %v verified %v", u.Email(), u.EmailVerifiedAt())
	}

	if _, err := inv.Accept(ctx, app.AcceptInput{Token: token, Username: "again", Password: fresh, Meta: meta}); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("second use: %v", err)
	}

	got := strings.Join(e.actions(t), ",")
	for _, a := range []string{domain.ActionInvitationCreate, domain.ActionInvitationRedeem, domain.ActionUserCreate} {
		if !strings.Contains(got, a) {
			t.Errorf("audit misses %s: %s", a, got)
		}
	}
}

func mustDevice(t *testing.T, s string) domain.DeviceID {
	t.Helper()

	d, err := domain.NewDeviceID(s)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestInvitationRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	e.addUser(t, "taken", "taken@example.org", domain.RoleListener)
	root := e.actor(t, "root")
	inv := e.invitations(&recNotifier{disabled: true})
	meta := app.RequestMeta{IP: ip}

	if _, err := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleAdmin, Device: "rtl-1"}); !errors.Is(err, domain.ErrInvalidInvitation) {
		t.Errorf("admin with device: %v", err)
	}

	if _, err := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleListener, TTL: 31 * 24 * time.Hour}); !errors.Is(err, domain.ErrInvalidInvitation) {
		t.Errorf("too long: %v", err)
	}

	// Without mail, an address is kept but the link is to copy.
	created, _ := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleListener, Email: "taken@example.org"})
	if created.Mailed || created.Invitation.Delivery() != domain.DeliveryLink {
		t.Errorf("created = %+v", created)
	}

	// The invited address belongs to an account: the invitation is
	// refused like an invalid one (SR-06).
	if _, err := inv.Accept(ctx, app.AcceptInput{Token: tokenOf(t, created.Link), Username: "other", Password: fresh, Meta: meta}); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("taken invited address: %v", err)
	}

	listener, _ := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleListener})
	token := tokenOf(t, listener.Link)

	for name, in := range map[string]app.AcceptInput{
		"taken username": {Token: token, Username: "TAKEN", Password: fresh, Meta: meta},
		"short password": {Token: token, Username: "x1", Password: "short", Meta: meta},
	} {
		if _, err := inv.Accept(ctx, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	if err := inv.Revoke(ctx, root, listener.Invitation.ID()); err != nil {
		t.Fatal(err)
	}

	if _, err := inv.Check(ctx, token, meta); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("revoked: %v", err)
	}

	if err := inv.Revoke(ctx, root, listener.Invitation.ID()); !errors.Is(err, domain.ErrInvitationNotPending) {
		t.Errorf("revoke twice: %v", err)
	}

	expiring, _ := inv.Create(ctx, root, app.CreateInvitationInput{Role: domain.RoleListener, TTL: time.Hour})
	e.clock.Advance(time.Hour)

	if _, err := inv.Check(ctx, tokenOf(t, expiring.Link), meta); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("expired: %v", err)
	}

	list, _ := inv.List(ctx)
	states := []string{}

	for _, i := range list {
		states = append(states, string(i.StateAt(inv.Now())))
	}

	slices.Sort(states)

	if strings.Join(states, ",") != "expired,pending,revoked" {
		t.Errorf("states = %v", states)
	}

	if _, err := inv.TestMail(ctx, root); !errors.Is(err, app.ErrMailDisabled) {
		t.Errorf("test mail without mail: %v", err)
	}
}
