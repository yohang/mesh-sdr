package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/argon2"
	"github.com/yohang/mesh-sdr/internal/identity/infra/memory"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var (
	t0    = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cheap = argon2.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}
	ip    = netip.MustParseAddr("192.0.2.10")
)

const password = "correct horse battery"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// spyHasher counts verifications.
type spyHasher struct {
	*argon2.Hasher

	mu       sync.Mutex
	verifies int
}

func (h *spyHasher) Verify(ctx context.Context, pw string, hash domain.PasswordHash) (bool, error) {
	h.mu.Lock()
	h.verifies++
	h.mu.Unlock()

	return h.Hasher.Verify(ctx, pw, hash)
}

func (h *spyHasher) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := h.verifies
	h.verifies = 0

	return n
}

type env struct {
	db       db.Adapter
	clock    *clock
	hasher   *spyHasher
	auth     *app.Auth
	admin    *app.UserAdmin
	users    *sqlite.Users
	sessions *sqlite.Sessions
	audit    *sqlite.AuditLog
	unknown  *memory.Throttle
}

func newEnv(t *testing.T, ipLimiter app.IPLimiter) *env {
	t.Helper()

	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	c := &clock{now: t0}
	ids := shared.NewUUIDv7Generator()
	logger := slog.New(slog.DiscardHandler)
	e := &env{
		db: a, clock: c, hasher: &spyHasher{Hasher: argon2.New(cheap, 4, 64)},
		users: sqlite.NewUsers(a, ids), sessions: sqlite.NewSessions(a), audit: sqlite.NewAuditLog(a),
		unknown: memory.NewThrottle(100),
	}

	local, err := app.NewLocalProvider(ctx, e.users, a, e.hasher, c.Now, logger)
	if err != nil {
		t.Fatal(err)
	}

	if ipLimiter == nil {
		ipLimiter = memory.NewIPLimiter(time.Millisecond, 1000, 100)
	}

	e.auth = app.NewAuth(app.AuthDeps{
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: a, IDs: ids, Now: c.Now, Provider: local,
		IPLimiter: ipLimiter, Unknown: e.unknown, Refusals: memory.NewRefusalGate(100), SessionPolicies: app.DefaultSessionPolicies(), Logger: logger,
	})
	e.admin = app.NewUserAdmin(app.UserAdminDeps{
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: a, Hasher: e.hasher, IDs: ids, Now: c.Now,
		Policy: app.NewPolicies(nil, nil), Logger: logger,
	})

	return e
}

func (e *env) addUser(t *testing.T, name, email string, role domain.Role) *domain.User {
	t.Helper()

	res, err := e.admin.Add(context.Background(), app.AddUserInput{Username: name, Email: email, Role: role, Password: password})
	if err != nil {
		t.Fatal(err)
	}

	e.hasher.count()

	return res.User
}

func (e *env) login(login, pw string) (app.LoginResult, error) {
	return e.auth.Login(context.Background(), app.LoginInput{Login: login, Password: pw, Meta: app.RequestMeta{IP: ip, UserAgent: "ua"}})
}

func (e *env) actions(t *testing.T) []string {
	t.Helper()

	entries, err := e.audit.Recent(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, len(entries))
	for i, en := range entries {
		out[len(entries)-1-i] = en.Action() + ":" + en.After()["reason"]
	}

	return out
}

func TestLoginOpensASession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "alice@example.org", domain.RoleAdmin)

	for _, login := range []string{"alice", "ALICE", " Alice@Example.org "} {
		res, err := e.login(login, password)
		if err != nil {
			t.Fatalf("login %q: %v", login, err)
		}

		if res.Principal.UserID() != u.ID() || res.Principal.Role() != domain.RoleAdmin {
			t.Errorf("principal = %v %v", res.Principal.UserID(), res.Principal.Role())
		}

		got, err := e.auth.Resolve(ctx, res.Token.Cookie())
		if err != nil || got.Session.ID() != res.Session.ID() || got.Principal.UserID() != u.ID() {
			t.Errorf("resolve: %v", err)
		}
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if !stored.LastLoginAt().Equal(t0) {
		t.Errorf("last login = %v", stored.LastLoginAt())
	}

	if got := e.actions(t); got[len(got)-1] != domain.ActionLoginSuccess+":" {
		t.Errorf("audit = %v", got)
	}
}

func TestLoginRotatesThePreviousSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	first, err := e.login("alice", password)
	if err != nil {
		t.Fatal(err)
	}

	second, err := e.auth.Login(ctx, app.LoginInput{Login: "alice", Password: password, Previous: first.Token.Cookie()})
	if err != nil {
		t.Fatal(err)
	}

	if first.Token == second.Token {
		t.Fatal("session id not rotated")
	}

	if _, err := e.auth.Resolve(ctx, first.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("previous session still valid: %v", err)
	}

	s, _ := e.sessions.ByTokenHash(ctx, first.Token.Hash())
	if s.RevokeReason() != domain.RevokeRotated {
		t.Errorf("revoke reason = %s", s.RevokeReason())
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	e.addUser(t, "bob", "", domain.RoleListener)

	if _, err := e.admin.Disable(ctx, "bob"); err != nil {
		t.Fatal(err)
	}

	tests := []struct{ name, login, password string }{
		{"wrong password", "alice", "wrong password!"},
		{"unknown user", "nobody", password},
		{"unknown e-mail", "nobody@example.org", password},
		{"disabled user", "bob", password},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.login(tt.login, tt.password)
			if !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Errorf("err = %v", err)
			}

			var de *shared.Error
			if !errors.As(err, &de) || de.Message() != domain.ErrInvalidCredentials.Message() {
				t.Errorf("message differs: %v", err)
			}

			// SR-06: one Argon2id verification, account or not.
			if n := e.hasher.count(); n != 1 {
				t.Errorf("%d verifications, want 1", n)
			}
		})
	}
}

func TestOversizedLoginInputIsRefusedBeforeVerification(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	for name, in := range map[string][2]string{
		"empty login":   {"   ", password},
		"long login":    {strings.Repeat("a", 255), password},
		"long password": {"alice", strings.Repeat("p", app.MaxPasswordBytes+1)},
	} {
		if _, err := e.login(in[0], in[1]); !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Errorf("%s: %v", name, err)
		}
	}

	if n := e.hasher.count(); n != 0 {
		t.Errorf("%d verifications for malformed input", n)
	}
}

func TestLoginThrottling(t *testing.T) {
	for _, login := range []string{"alice", "nobody"} {
		t.Run(login, func(t *testing.T) {
			e := newEnv(t, nil)
			e.addUser(t, "alice", "", domain.RoleListener)

			for i := range 5 {
				if _, err := e.login(login, "wrong password!"); !errors.Is(err, domain.ErrInvalidCredentials) {
					t.Fatalf("failure %d: %v", i+1, err)
				}
			}

			_, err := e.login(login, password)

			var rl *domain.RateLimitError
			if !errors.As(err, &rl) || rl.RetryAfter() != time.Second {
				t.Fatalf("6th attempt: %v", err)
			}

			if n := e.hasher.count(); n != 5 {
				t.Errorf("throttled attempt verified a password (%d verifications)", n)
			}

			e.clock.Advance(time.Second)

			for i := range 5 {
				if i > 0 {
					e.clock.Advance(time.Minute)
				}

				_, _ = e.login(login, "wrong password!")
			}

			_, err = e.login(login, password)
			if !errors.As(err, &rl) || rl.RetryAfter() != 15*time.Minute {
				t.Fatalf("after 10 failures: %v", err)
			}

			e.clock.Advance(15 * time.Minute)

			_, err = e.login(login, password)
			if login == "alice" && err != nil {
				t.Errorf("login after the lock: %v", err)
			}

			if login == "nobody" && !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Errorf("unknown login after the lock: %v", err)
			}
		})
	}
}

// Parallel guesses cannot exceed the throttle: each attempt is reserved
// atomically before the password is verified.
func TestConcurrentGuessesAreBounded(t *testing.T) {
	for _, login := range []string{"alice", "nobody"} {
		t.Run(login, func(t *testing.T) {
			e := newEnv(t, nil)
			e.addUser(t, "alice", "", domain.RoleListener)

			const n = 20

			errs := make(chan error, n)
			start := make(chan struct{})

			var wg sync.WaitGroup

			for range n {
				wg.Go(func() {
					<-start

					_, err := e.login(login, "wrong password!")
					errs <- err
				})
			}

			close(start)
			wg.Wait()
			close(errs)

			invalid, limited := 0, 0

			for err := range errs {
				switch {
				case errors.Is(err, domain.ErrInvalidCredentials):
					invalid++
				case errors.Is(err, domain.ErrRateLimited):
					limited++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}

			// The clock does not move: the 5th failure delays every later
			// attempt.
			if invalid != 5 || limited != n-5 {
				t.Errorf("%d invalid, %d rate limited; want 5 and %d", invalid, limited, n-5)
			}

			if v := e.hasher.count(); v != 5 {
				t.Errorf("%d password verifications, want 5", v)
			}
		})
	}
}

func TestLoginLockoutIsPersistedAndAudited(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "", domain.RoleListener)

	for range 10 {
		_, _ = e.login("alice", "wrong password!")
		e.clock.Advance(time.Minute)
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if stored.FailedLogins() != 10 || stored.LockedUntil().IsZero() {
		t.Errorf("throttle state = %d %v", stored.FailedLogins(), stored.LockedUntil())
	}

	found := false
	for _, a := range e.actions(t) {
		found = found || a == domain.ActionLoginLockout+":"
	}

	if !found {
		t.Errorf("no lock-out audit entry: %v", e.actions(t))
	}
}

func TestLoginIPRateLimit(t *testing.T) {
	e := newEnv(t, memory.NewIPLimiter(12*time.Second, 5, 100))
	e.addUser(t, "alice", "", domain.RoleListener)

	for range 5 {
		if _, err := e.login("alice", password); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := e.login("alice", password); !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("6th attempt from one address: %v", err)
	}
}

// A flood of refused logins writes one audit row per key and window.
func TestRefusedLoginsAreAuditedOncePerWindow(t *testing.T) {
	count := func(e *env, entry string) int {
		n := 0
		for _, a := range e.actions(t) {
			if a == entry {
				n++
			}
		}

		return n
	}

	t.Run("address", func(t *testing.T) {
		e := newEnv(t, memory.NewIPLimiter(time.Minute, 1, 100))
		e.addUser(t, "alice", "", domain.RoleListener)

		for range 20 {
			_, _ = e.login("alice", password)
		}

		if n := count(e, domain.ActionLoginFailure+":ip_rate_limited"); n != 1 {
			t.Errorf("%d audit rows for refused addresses, want 1", n)
		}

		e.clock.Advance(time.Minute)
		_, _ = e.login("alice", password)
		_, _ = e.login("alice", password)

		if n := count(e, domain.ActionLoginFailure+":ip_rate_limited"); n != 2 {
			t.Errorf("%d audit rows after a new window, want 2", n)
		}
	})

	t.Run("account", func(t *testing.T) {
		e := newEnv(t, nil)
		e.addUser(t, "alice", "", domain.RoleListener)

		for range 30 {
			_, _ = e.login("alice", "wrong password!")
		}

		if n := count(e, domain.ActionLoginFailure+":throttled"); n != 1 {
			t.Errorf("%d audit rows for throttled attempts, want 1", n)
		}

		if n := count(e, domain.ActionLoginFailure+":invalid_credentials"); n != 5 {
			t.Errorf("%d audit rows for wrong passwords, want 5", n)
		}
	})
}

func TestLoginRehashesOutdatedHashes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "", domain.RoleListener)

	old, err := argon2.New(argon2.Params{MemoryKiB: 32, Iterations: 2, Parallelism: 1}, 1, 1).Hash(ctx, password)
	if err != nil {
		t.Fatal(err)
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	_ = stored.ReplacePasswordHash(old, t0)

	if err := e.users.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if _, err := e.login("alice", password); err != nil {
		t.Fatal(err)
	}

	after, _ := e.users.ByID(ctx, u.ID())
	if after.PasswordHash() == old || e.hasher.NeedsRehash(after.PasswordHash()) {
		t.Errorf("hash not upgraded: %s", after.PasswordHash())
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	res, err := e.login("alice", password)
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "garbage", domain.NewSessionToken().Cookie()} {
		if _, err := e.auth.Resolve(ctx, bad); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("Resolve(%q) = %v", bad, err)
		}
	}

	e.clock.Advance(23 * time.Hour)

	if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); err != nil {
		t.Fatalf("active session: %v", err)
	}

	stored, _ := e.sessions.ByTokenHash(ctx, res.Token.Hash())
	if !stored.LastSeenAt().Equal(t0.Add(23 * time.Hour)) {
		t.Errorf("activity not recorded: %v", stored.LastSeenAt())
	}

	// The absolute lifetime (24 h without "remember me") wins over activity.
	e.clock.Advance(time.Hour)

	if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("expired session: %v", err)
	}
}

func TestRememberMeAndIdleExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	res, err := e.auth.Login(ctx, app.LoginInput{Login: "alice", Password: password, Remember: true})
	if err != nil {
		t.Fatal(err)
	}

	for range 10 {
		e.clock.Advance(20 * time.Hour)

		if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); err != nil {
			t.Fatalf("remember-me session expired early: %v", err)
		}
	}

	e.clock.Advance(25 * time.Hour)

	if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("idle session: %v", err)
	}
}

func TestLogout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	res, err := e.login("alice", password)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := e.auth.Logout(ctx, res.Token.Cookie(), app.RequestMeta{IP: ip}); err != nil {
			t.Fatal(err)
		}
	}

	if err := e.auth.Logout(ctx, "garbage", app.RequestMeta{}); err != nil {
		t.Error(err)
	}

	if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("session after logout: %v", err)
	}

	s, _ := e.sessions.ByTokenHash(ctx, res.Token.Hash())
	if s.RevokeReason() != domain.RevokeLogout {
		t.Errorf("revoke reason = %s", s.RevokeReason())
	}

	n := 0
	for _, a := range e.actions(t) {
		if a == domain.ActionLogout+":" {
			n++
		}
	}

	if n != 1 {
		t.Errorf("%d logout audit entries, want 1", n)
	}
}

func TestUserAdmin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)

	res, err := e.admin.Add(ctx, app.AddUserInput{Username: "root", Role: domain.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.GeneratedPassword) < 20 || !res.User.MustChangePassword() || res.User.Role() != domain.RoleAdmin {
		t.Errorf("generated account = %q must change %v role %v", res.GeneratedPassword, res.User.MustChangePassword(), res.User.Role())
	}

	if _, err := e.login("root", res.GeneratedPassword); err != nil {
		t.Errorf("login with the generated password: %v", err)
	}

	if _, err := e.admin.Add(ctx, app.AddUserInput{Username: "ROOT", Password: password}); !errors.Is(err, domain.ErrUsernameTaken) {
		t.Errorf("duplicate: %v", err)
	}

	if _, err := e.admin.Add(ctx, app.AddUserInput{Username: "x", Password: password}); !errors.Is(err, domain.ErrInvalidUsername) {
		t.Errorf("bad name: %v", err)
	}

	if _, err := e.admin.Add(ctx, app.AddUserInput{Username: "short", Password: "short"}); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("short password: %v", err)
	}

	if _, err := e.admin.Add(ctx, app.AddUserInput{Username: "op", Role: domain.Role(15), Password: password}); !errors.Is(err, domain.ErrInvalidRole) {
		t.Errorf("bad role: %v", err)
	}

	for name, want := range map[string]bool{"root": true, "Root": true, "nobody": false, "!": false} {
		if got, err := e.admin.Exists(ctx, name); err != nil || got != want {
			t.Errorf("Exists(%s) = %v, %v", name, got, err)
		}
	}
}

func TestDisableRevokesSessionsAndEnableRestores(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleOperator)

	s1, _ := e.login("alice", password)
	s2, _ := e.login("alice", password)

	res, err := e.admin.Disable(ctx, "Alice")
	if err != nil || !res.Changed || res.RevokedSessions != 2 {
		t.Fatalf("disable = %+v, %v", res, err)
	}

	for _, s := range []app.LoginResult{s1, s2} {
		if _, err := e.auth.Resolve(ctx, s.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("session of a disabled user: %v", err)
		}
	}

	if res, _ := e.admin.Disable(ctx, "alice"); res.Changed {
		t.Error("second disable changed something")
	}

	if _, err := e.login("alice", password); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("login of a disabled user: %v", err)
	}

	if changed, err := e.admin.Enable(ctx, "alice"); err != nil || !changed {
		t.Fatalf("enable = %v, %v", changed, err)
	}

	if changed, _ := e.admin.Enable(ctx, "alice"); changed {
		t.Error("second enable changed something")
	}

	if _, err := e.login("alice", password); err != nil {
		t.Errorf("login after enable: %v", err)
	}

	for _, f := range []func() error{
		func() error { _, err := e.admin.Disable(ctx, "nobody"); return err },
		func() error { _, err := e.admin.Enable(ctx, "nobody"); return err },
		func() error { _, err := e.admin.Disable(ctx, "!"); return err },
	} {
		if err := f(); !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("unknown user: %v", err)
		}
	}
}

func TestSessionReaper(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)

	old, _ := e.login("alice", password)

	e.clock.Advance(32 * 24 * time.Hour)

	fresh, _ := e.login("alice", password)

	n, err := app.NewSessionReaper(e.sessions, app.FixedRetention{Sessions: app.DefaultSessionRetention}, e.clock.Now).Reap(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v", n, err)
	}

	if _, err := e.sessions.ByTokenHash(ctx, old.Token.Hash()); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Error("old session kept")
	}

	if _, err := e.sessions.ByTokenHash(ctx, fresh.Token.Hash()); err != nil {
		t.Error("fresh session deleted")
	}
}

func TestAuditPurger(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	now := e.clock.Now()

	for _, age := range []time.Duration{40 * 24 * time.Hour, 31 * 24 * time.Hour, 29 * 24 * time.Hour, time.Hour} {
		entry, err := domain.NewAuditEntry(now.Add(-age), domain.SystemActor(), "test.event", domain.ResultOK)
		if err != nil {
			t.Fatal(err)
		}

		if err := e.audit.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	// Entries older than the retention are deleted, the others kept.
	p := app.NewAuditPurger(e.audit, app.FixedRetention{Audit: 30 * 24 * time.Hour}, e.clock.Now)
	if p.Name() != "audit.purge" {
		t.Errorf("name = %s", p.Name())
	}

	n, err := p.Run(ctx)
	if err != nil || n != 2 {
		t.Fatalf("purged %d, %v", n, err)
	}

	// The two kept entries, and the record of the purge.
	left, err := e.audit.Recent(ctx, 10)
	if err != nil || len(left) != 3 {
		t.Fatalf("left %d entries, %v", len(left), err)
	}

	if r := left[0]; r.Action() != "retention.purge" || r.Actor().Kind() != domain.ActorSystem || r.After()["rows_deleted"] != "2" {
		t.Errorf("purge record = %s %s %v", r.Action(), r.Actor().Kind(), r.After())
	}

	// Nothing to delete: nothing recorded.
	if n, err := p.Run(ctx); err != nil || n != 0 {
		t.Fatalf("second purge = %d, %v", n, err)
	}

	if left, _ := e.audit.Recent(ctx, 10); len(left) != 3 {
		t.Errorf("an empty purge was recorded: %d entries", len(left))
	}
}
