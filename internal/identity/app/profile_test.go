package app_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func (e *env) profile(n app.Notifier) *app.Profile {
	return app.NewProfile(app.ProfileDeps{
		Pending: e.pending(),
		Users:   e.users, Tokens: sqlite.NewEmailChanges(e.db), Audit: e.audit, Tx: e.db, IDs: shared.NewUUIDv7Generator(),
		Now: e.clock.Now, Passwords: e.passwords(), Notifier: n, Links: app.NewLinks("https://hub.example"),
		Logger: slog.New(slog.DiscardHandler),
	})
}

func TestEmailChangeWithoutMail(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	p := e.profile(&recNotifier{disabled: true})
	me := e.actor(t, "alice")

	res, err := p.ChangeEmail(ctx, me, "alice@example.org", password)
	if err != nil || res.Pending {
		t.Fatalf("change = %+v, %v", res, err)
	}

	u, _ := p.Me(ctx, me)
	if u.Email().String() != "alice@example.org" || !u.EmailVerifiedAt().IsZero() {
		t.Errorf("e-mail = %v verified %v", u.Email(), u.EmailVerifiedAt())
	}

	if _, err := p.ChangeEmail(ctx, me, "bad", password); !errors.Is(err, domain.ErrInvalidEmail) {
		t.Errorf("bad address: %v", err)
	}

	if _, err := p.ChangeEmail(ctx, me, "", "wrong password"); !errors.Is(err, domain.ErrInvalidCurrentPassword) {
		t.Errorf("wrong password: %v", err)
	}

	if _, err := p.ChangeEmail(ctx, me, "", password); err != nil {
		t.Fatal(err)
	}

	if u, _ := p.Me(ctx, me); !u.Email().IsZero() {
		t.Error("address not removed")
	}
}

func TestEmailChangeWithMail(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	n := &recNotifier{}
	p := e.profile(n)
	me := e.actor(t, "alice")

	res, err := p.ChangeEmail(ctx, me, "next@example.org", password)
	if err != nil || !res.Pending {
		t.Fatalf("change = %+v, %v", res, err)
	}

	got := n.all()
	if len(got) != 1 || got[0].kind != "email_confirmation" || got[0].to != "next@example.org" ||
		!strings.HasPrefix(got[0].link, "https://hub.example/account/email/verify/") {
		t.Fatalf("notifications = %v", got)
	}

	token := strings.TrimPrefix(got[0].link, "https://hub.example/account/email/verify/")

	if err := p.CheckEmailToken(ctx, token); err != nil {
		t.Fatal(err)
	}

	if err := p.ConfirmEmail(ctx, token, app.RequestMeta{IP: ip}); err != nil {
		t.Fatal(err)
	}

	u, _ := p.Me(ctx, me)
	if u.Email().String() != "next@example.org" || u.EmailVerifiedAt().IsZero() {
		t.Errorf("e-mail = %v", u.Email())
	}

	if got := n.all(); len(got) != 2 || got[1].kind != "email_changed" || got[1].to != "alice@example.org" {
		t.Errorf("notice = %v", got)
	}

	if err := p.ConfirmEmail(ctx, token, app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("second use: %v", err)
	}

	if err := p.CheckEmailToken(ctx, "garbage"); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("garbage: %v", err)
	}
}

func TestDisplayName(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	p := e.profile(nil)
	me := e.actor(t, "alice")

	if u, err := p.SetDisplayName(ctx, me, "Alice"); err != nil || u.DisplayName().String() != "Alice" {
		t.Errorf("set = %v", err)
	}

	if _, err := p.SetDisplayName(ctx, me, "\x00"); !errors.Is(err, domain.ErrInvalidDisplayName) {
		t.Errorf("control character: %v", err)
	}

	if u, err := p.SetDisplayName(ctx, me, ""); err != nil || !u.DisplayName().IsZero() {
		t.Errorf("clear = %v", err)
	}
}
