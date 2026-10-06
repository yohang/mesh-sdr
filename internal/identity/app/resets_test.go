package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
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

func (e *env) resets(n app.Notifier, rv app.RevocationPublisher) *app.Resets {
	return app.NewResets(app.ResetsDeps{
		Pending: e.pending(),
		Tokens:  sqlite.NewPasswordResets(e.db), Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: e.db, Hasher: e.hasher,
		IDs: shared.NewUUIDv7Generator(), Now: e.clock.Now, Settings: settings.Defaults{}, Policies: app.NewPolicies(nil, nil),
		Notifier: n, Links: app.NewLinks("https://hub.example"),
		Requests: memory.NewIPLimiter(time.Hour, 3, 10), Accounts: memory.NewKeyLimiter(time.Hour, 2, 10),
		Confirms: memory.NewIPLimiter(time.Millisecond, 1000, 10), Revocations: rv,
		Async: func(f func()) { f() }, Logger: slog.New(slog.DiscardHandler),
	})
}

func resetToken(t *testing.T, link string) string {
	t.Helper()

	tok, ok := strings.CutPrefix(link, "https://hub.example/password/reset/")
	if !ok {
		t.Fatalf("link = %q", link)
	}

	return tok
}

func TestPasswordResetByLink(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	n := &recNotifier{}
	rv := &recRevocations{}
	r := e.resets(n, rv)
	meta := app.RequestMeta{IP: ip}

	in, _ := e.login("alice", password)

	// Lock the account: a lock-out never blocks a reset (SR-05).
	for range 10 {
		_, _ = e.login("alice", "wrong password")
	}

	for _, login := range []string{"ALICE@example.org", "nobody"} {
		if err := r.Request(ctx, login, meta); err != nil {
			t.Fatalf("request %s: %v", login, err)
		}
	}

	got := n.all()
	if len(got) != 1 || got[0].kind != "password_reset" || got[0].to != "alice@example.org" {
		t.Fatalf("notifications = %v", got)
	}

	token := resetToken(t, got[0].link)

	if err := r.Check(ctx, token, meta); err != nil {
		t.Fatal(err)
	}

	if err := r.Confirm(ctx, token, "short", meta); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("short: %v", err)
	}

	if err := r.Confirm(ctx, token, fresh, meta); err != nil {
		t.Fatal(err)
	}

	if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Error("session survived the reset")
	}

	if _, err := e.login("alice", fresh); err != nil {
		t.Errorf("login with the new password (lock-out cleared): %v", err)
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if stored.EmailVerifiedAt().IsZero() {
		t.Error("address not confirmed by the reset")
	}

	if err := r.Confirm(ctx, token, fresh+"2", meta); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("second use: %v", err)
	}

	if len(rv.got) != 1 || rv.got[0].Users[0] != u.ID() {
		t.Errorf("revocations = %v", rv.got)
	}

	all := strings.Join(e.actions(t), ",")
	if !strings.Contains(all, domain.ActionResetRequest) || !strings.Contains(all, domain.ActionResetComplete) {
		t.Errorf("audit = %s", all)
	}
}

func TestPasswordResetRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	e.addUser(t, "nomail", "", domain.RoleListener)
	n := &recNotifier{}
	r := e.resets(n, nil)

	// A newer link invalidates the older one.
	_ = r.Request(ctx, "alice", app.RequestMeta{IP: ip})
	_ = r.Request(ctx, "alice", app.RequestMeta{IP: ip})

	got := n.all()
	if len(got) != 2 {
		t.Fatalf("notifications = %v", got)
	}

	if err := r.Check(ctx, resetToken(t, got[0].link), app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("superseded: %v", err)
	}

	// Per client address: 3 per hour, then 429; per account: silently.
	if err := r.Request(ctx, "nomail", app.RequestMeta{IP: ip}); err != nil {
		t.Fatal(err)
	}

	var rl *domain.RateLimitError
	if err := r.Request(ctx, "alice", app.RequestMeta{IP: ip}); !errors.As(err, &rl) {
		t.Errorf("4th request: %v", err)
	}

	other := app.RequestMeta{IP: mustIP("198.51.100.7")}
	if err := r.Request(ctx, "alice", other); err != nil {
		t.Fatal(err)
	}

	if len(n.all()) != 2 {
		t.Errorf("account limit: %d messages", len(n.all()))
	}

	// Expiry.
	if err := r.Check(ctx, resetToken(t, got[1].link), app.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(31 * time.Minute)

	if err := r.Check(ctx, resetToken(t, got[1].link), app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("expired: %v", err)
	}

	// Without mail, nothing is sent and the answer is the same.
	off := e.resets(&recNotifier{disabled: true}, nil)
	if err := off.Request(ctx, "alice", app.RequestMeta{IP: mustIP("203.0.113.9")}); err != nil {
		t.Errorf("request without mail: %v", err)
	}
}

func TestAdminIssuedReset(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	withMail := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	noMail := e.addUser(t, "bob", "", domain.RoleListener)
	root := e.actor(t, "root")
	n := &recNotifier{}
	r := e.resets(n, nil)

	if res, err := r.IssueByAdmin(ctx, root, withMail.ID()); err != nil || !res.Mailed || res.Link != "" {
		t.Errorf("with an address = %+v, %v", res, err)
	}

	res, err := r.IssueByAdmin(ctx, root, noMail.ID())
	if err != nil || res.Mailed || !strings.HasPrefix(res.Link, "https://hub.example/password/reset/") {
		t.Errorf("without an address = %+v, %v", res, err)
	}

	if err := r.Confirm(ctx, resetToken(t, res.Link), fresh, app.RequestMeta{}); err != nil {
		t.Errorf("confirm: %v", err)
	}

	if _, err := r.IssueByAdmin(ctx, root, domain.UserID{}); !errors.Is(err, domain.ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func mustIP(s string) netip.Addr { return netip.MustParseAddr(s) }
