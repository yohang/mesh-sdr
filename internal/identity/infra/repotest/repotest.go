// Package repotest holds the contract suites of the identity repositories
// (ADR 0006): every dialect adapter runs them against a migrated database.
package repotest

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// AuditReader is the audit repository with its read side.
type AuditReader interface {
	domain.AuditLog
	Recent(ctx context.Context, limit int) ([]domain.AuditEntry, error)
}

// Repos are the repositories of one fresh, migrated database.
type Repos struct {
	Users    domain.UserRepository
	Sessions domain.SessionRepository
	Audit    AuditReader
}

// Factory opens fresh repositories for a test.
type Factory func(t *testing.T) Repos

var (
	t0  = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ids = shared.NewUUIDv7Generator()
)

func userID(t *testing.T) domain.UserID {
	t.Helper()

	u, err := ids.New(t0)
	if err != nil {
		t.Fatal(err)
	}

	id, err := domain.NewUserID(u)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func sessionID(t *testing.T) domain.SessionID {
	t.Helper()

	u, err := ids.New(t0)
	if err != nil {
		t.Fatal(err)
	}

	id, err := domain.NewSessionID(u)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

// NewUser builds an unsaved local user named name with e-mail email
// (optional) and the given grants.
func NewUser(t *testing.T, name, email string, grants ...domain.RoleGrant) *domain.User {
	t.Helper()

	username, err := domain.NewUsername(name)
	if err != nil {
		t.Fatal(err)
	}

	var mail domain.Email
	if email != "" {
		if mail, err = domain.NewEmail(email); err != nil {
			t.Fatal(err)
		}
	}

	hash, _ := domain.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA")
	display, _ := domain.NewDisplayName("Display " + name)

	u, err := domain.NewLocalUser(domain.NewLocalUserParams{
		ID: userID(t), Username: username, Email: mail, DisplayName: display, PasswordHash: hash,
		MustChangePassword: true, Grants: grants, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// RunUsers checks the UserRepository contract.
func RunUsers(t *testing.T, open Factory) {
	ctx := context.Background()
	dev, _ := domain.NewDeviceID("rtl-1")
	op, _ := domain.NewRoleGrant(domain.RoleOperator, dev)
	admin, _ := domain.NewRoleGrant(domain.RoleAdmin, domain.DeviceID{})

	t.Run("add and load", func(t *testing.T) {
		r := open(t).Users
		u := NewUser(t, "Alice", "Alice@Example.org", op, admin)

		if err := r.Add(ctx, u); err != nil {
			t.Fatal(err)
		}

		got, err := r.ByID(ctx, u.ID())
		if err != nil {
			t.Fatal(err)
		}

		if got.Username() != u.Username() || got.Email() != u.Email() || got.DisplayName() != u.DisplayName() ||
			got.PasswordHash() != u.PasswordHash() || !got.MustChangePassword() || !got.Enabled() ||
			got.Version() != 1 || got.Origin() != domain.OriginDB || !got.CreatedAt().Equal(t0) {
			t.Errorf("round trip differs: %+v", got)
		}

		if ids := got.Identities(); len(ids) != 1 || ids[0] != domain.LocalIdentity(u.ID()) {
			t.Errorf("identities = %v", ids)
		}

		if g := got.Grants(); len(g) != 2 || got.Role() != domain.RoleAdmin {
			t.Errorf("grants = %v", g)
		}

		for name, load := range map[string]func() (*domain.User, error){
			"username": func() (*domain.User, error) {
				n, _ := domain.NewUsername("ALICE")

				return r.ByUsername(ctx, n)
			},
			"login username": func() (*domain.User, error) {
				l, _ := domain.NewLogin("aLiCe")

				return r.ByLogin(ctx, l)
			},
			"login e-mail": func() (*domain.User, error) {
				l, _ := domain.NewLogin("alice@EXAMPLE.org")

				return r.ByLogin(ctx, l)
			},
			"identity": func() (*domain.User, error) { return r.ByIdentity(ctx, domain.LocalIdentity(u.ID())) },
		} {
			if got, err := load(); err != nil || got.ID() != u.ID() {
				t.Errorf("by %s = %v, %v", name, got, err)
			}
		}
	})

	t.Run("not found", func(t *testing.T) {
		r := open(t).Users
		n, _ := domain.NewUsername("nobody")
		l, _ := domain.NewLogin("nobody@example.org")
		other, _ := domain.NewProviderID("oidc:x")
		i, _ := domain.NewIdentity(other, "sub")

		for name, err := range map[string]error{
			"id":       second(r.ByID(ctx, userID(t))),
			"username": second(r.ByUsername(ctx, n)),
			"login":    second(r.ByLogin(ctx, l)),
			"identity": second(r.ByIdentity(ctx, i)),
		} {
			if !errors.Is(err, domain.ErrUserNotFound) {
				t.Errorf("by %s: %v", name, err)
			}
		}
	})

	t.Run("uniqueness", func(t *testing.T) {
		r := open(t).Users
		if err := r.Add(ctx, NewUser(t, "bob", "bob@example.org")); err != nil {
			t.Fatal(err)
		}

		if err := r.Add(ctx, NewUser(t, "BOB", "")); !errors.Is(err, domain.ErrUsernameTaken) {
			t.Errorf("duplicate username: %v", err)
		}

		if err := r.Add(ctx, NewUser(t, "bobby", "BOB@example.org")); !errors.Is(err, domain.ErrEmailTaken) {
			t.Errorf("duplicate e-mail: %v", err)
		}

		// Users without e-mail do not collide.
		if err := r.Add(ctx, NewUser(t, "carol", "")); err != nil {
			t.Error(err)
		}

		if err := r.Add(ctx, NewUser(t, "dave", "")); err != nil {
			t.Error(err)
		}
	})

	t.Run("save with optimistic concurrency", func(t *testing.T) {
		r := open(t).Users
		u := NewUser(t, "erin", "")

		if err := r.Add(ctx, u); err != nil {
			t.Fatal(err)
		}

		stale, _ := r.ByID(ctx, u.ID())

		u.RecordLoginFailure(t0.Add(time.Minute), domain.NewThrottlePolicy(1, 2, time.Minute, time.Hour))
		u.Disable(t0.Add(time.Minute))

		if err := r.Save(ctx, u); err != nil {
			t.Fatal(err)
		}

		if u.Version() != 2 {
			t.Errorf("version after save = %d", u.Version())
		}

		got, _ := r.ByID(ctx, u.ID())
		if got.Enabled() || got.FailedLogins() != 1 || got.LockedUntil().IsZero() || got.Version() != 2 ||
			!got.UpdatedAt().Equal(t0.Add(time.Minute)) {
			t.Errorf("saved state = enabled %v failed %d locked %v version %d", got.Enabled(), got.FailedLogins(), got.LockedUntil(), got.Version())
		}

		stale.Disable(t0)

		if err := r.Save(ctx, stale); !errors.Is(err, domain.ErrVersionConflict) {
			t.Errorf("stale save: %v", err)
		}
	})
}

func second[T any](_ T, err error) error { return err }

// RunSessions checks the SessionRepository contract.
func RunSessions(t *testing.T, open Factory) {
	ctx := context.Background()

	setup := func(t *testing.T) (Repos, *domain.User) {
		t.Helper()

		r := open(t)
		u := NewUser(t, "alice", "")

		if err := r.Users.Add(ctx, u); err != nil {
			t.Fatal(err)
		}

		return r, u
	}

	start := func(t *testing.T, r Repos, u *domain.User, now time.Time) (*domain.Session, domain.SessionToken) {
		t.Helper()

		s, tok, err := domain.StartSession(domain.StartSessionParams{
			ID: sessionID(t), UserID: u.ID(), Provider: domain.ProviderLocal, Now: now,
			IP: netip.MustParseAddr("2001:db8::1"), UserAgent: "test-agent", Policy: domain.DefaultSessionPolicy(),
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := r.Sessions.Add(ctx, s); err != nil {
			t.Fatal(err)
		}

		return s, tok
	}

	t.Run("add, load, touch and revoke", func(t *testing.T) {
		r, u := setup(t)
		s, tok := start(t, r, u, t0)

		got, err := r.Sessions.ByTokenHash(ctx, tok.Hash())
		if err != nil {
			t.Fatal(err)
		}

		if got.ID() != s.ID() || got.UserID() != u.ID() || got.CSRFSecret() != s.CSRFSecret() ||
			got.IP() != s.IP() || got.UserAgent() != "test-agent" || got.Provider() != domain.ProviderLocal ||
			!got.IdleExpiresAt().Equal(s.IdleExpiresAt()) || !got.AbsoluteExpiresAt().Equal(s.AbsoluteExpiresAt()) {
			t.Errorf("round trip differs")
		}

		got.Touch(t0.Add(time.Hour), domain.DefaultSessionPolicy())

		if err := r.Sessions.Touch(ctx, got); err != nil {
			t.Fatal(err)
		}

		got.Revoke(domain.RevokeLogout, t0.Add(2*time.Hour))

		if err := r.Sessions.Revoke(ctx, got); err != nil {
			t.Fatal(err)
		}

		again, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())
		if !again.LastSeenAt().Equal(t0.Add(time.Hour)) || again.RevokeReason() != domain.RevokeLogout ||
			!again.RevokedAt().Equal(t0.Add(2*time.Hour)) {
			t.Errorf("saved state = last seen %v revoked %v (%s)", again.LastSeenAt(), again.RevokedAt(), again.RevokeReason())
		}

		if _, err := r.Sessions.ByTokenHash(ctx, domain.NewSessionToken().Hash()); !errors.Is(err, domain.ErrSessionNotFound) {
			t.Errorf("unknown token: %v", err)
		}
	})

	t.Run("a stale touch never resurrects a revoked session", func(t *testing.T) {
		r, u := setup(t)
		_, tok := start(t, r, u, t0)

		// Two requests read the session; one logs out, the other then
		// records activity from its stale copy.
		stale, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())
		current, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())

		current.Revoke(domain.RevokeLogout, t0.Add(time.Minute))

		if err := r.Sessions.Revoke(ctx, current); err != nil {
			t.Fatal(err)
		}

		if !stale.Touch(t0.Add(2*time.Minute), domain.DefaultSessionPolicy()) {
			t.Fatal("stale copy not touched")
		}

		if err := r.Sessions.Touch(ctx, stale); err != nil {
			t.Fatal(err)
		}

		got, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())
		if got.ActiveAt(t0.Add(3*time.Minute)) || got.RevokeReason() != domain.RevokeLogout ||
			!got.LastSeenAt().Equal(t0) {
			t.Errorf("session after a stale touch: revoked %v (%s), last seen %v", got.RevokedAt(), got.RevokeReason(), got.LastSeenAt())
		}
	})

	t.Run("revoke keeps the first revocation", func(t *testing.T) {
		r, u := setup(t)
		s, tok := start(t, r, u, t0)

		stale, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())

		s.Revoke(domain.RevokeLogout, t0.Add(time.Minute))

		if err := r.Sessions.Revoke(ctx, s); err != nil {
			t.Fatal(err)
		}

		stale.Revoke(domain.RevokeAdmin, t0.Add(time.Hour))

		if err := r.Sessions.Revoke(ctx, stale); err != nil {
			t.Fatal(err)
		}

		if _, err := r.Sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokeUserDisabled, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}

		got, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())
		if got.RevokeReason() != domain.RevokeLogout || !got.RevokedAt().Equal(t0.Add(time.Minute)) {
			t.Errorf("revocation = %v (%s)", got.RevokedAt(), got.RevokeReason())
		}

		unrevoked, _ := start(t, r, u, t0)
		if err := r.Sessions.Revoke(ctx, unrevoked); err == nil {
			t.Error("storing an unrevoked session as revoked accepted")
		}
	})

	t.Run("revoke all for user", func(t *testing.T) {
		r, u := setup(t)
		_, tok1 := start(t, r, u, t0)
		_, tok2 := start(t, r, u, t0)

		other := NewUser(t, "bob", "")
		if err := r.Users.Add(ctx, other); err != nil {
			t.Fatal(err)
		}

		_, tok3 := start(t, r, other, t0)

		n, err := r.Sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokeUserDisabled, t0.Add(time.Minute))
		if err != nil || n != 2 {
			t.Fatalf("revoked %d, %v", n, err)
		}

		for _, tok := range []domain.SessionToken{tok1, tok2} {
			s, _ := r.Sessions.ByTokenHash(ctx, tok.Hash())
			if s.ActiveAt(t0.Add(2*time.Minute)) || s.RevokeReason() != domain.RevokeUserDisabled {
				t.Error("session not revoked")
			}
		}

		if s, _ := r.Sessions.ByTokenHash(ctx, tok3.Hash()); !s.ActiveAt(t0.Add(2 * time.Minute)) {
			t.Error("another user's session was revoked")
		}

		if n, _ := r.Sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokeAdmin, t0.Add(time.Hour)); n != 0 {
			t.Errorf("revoked sessions revoked again: %d", n)
		}
	})

	t.Run("delete ended sessions", func(t *testing.T) {
		r, u := setup(t)
		_, live := start(t, r, u, t0.Add(40*24*time.Hour))
		_, expired := start(t, r, u, t0)
		revoked, revokedTok := start(t, r, u, t0.Add(39*24*time.Hour))

		revoked.Revoke(domain.RevokeLogout, t0.Add(39*24*time.Hour))
		if err := r.Sessions.Revoke(ctx, revoked); err != nil {
			t.Fatal(err)
		}

		cutoff := t0.Add(31 * 24 * time.Hour) // the first session expired a day after t0

		n, err := r.Sessions.DeleteEndedBefore(ctx, cutoff, 100)
		if err != nil || n != 1 {
			t.Fatalf("deleted %d, %v", n, err)
		}

		if _, err := r.Sessions.ByTokenHash(ctx, expired.Hash()); !errors.Is(err, domain.ErrSessionNotFound) {
			t.Error("expired session kept")
		}

		for _, tok := range []domain.SessionToken{live, revokedTok} {
			if _, err := r.Sessions.ByTokenHash(ctx, tok.Hash()); err != nil {
				t.Errorf("recent session deleted: %v", err)
			}
		}
	})
}

// RunAudit checks the AuditLog contract.
func RunAudit(t *testing.T, open Factory) {
	ctx := context.Background()
	r := open(t)
	u := NewUser(t, "alice", "")

	first, _ := domain.NewAuditEntry(t0, domain.UserActor(u.ID(), netip.MustParseAddr("::ffff:192.0.2.1")), domain.ActionLoginSuccess, domain.ResultOK)
	first = first.WithTarget("user", u.ID().String()).WithRequestID("req-1").WithAfter(map[string]string{"provider": "local"})
	second, _ := domain.NewAuditEntry(t0.Add(time.Second), domain.CLIActor(), domain.ActionUserDisable, domain.ResultOK)
	second = second.WithBefore(map[string]string{"enabled": "true"})

	for _, e := range []domain.AuditEntry{first, second} {
		if err := r.Audit.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Audit.Recent(ctx, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("recent = %d, %v", len(got), err)
	}

	if got[0].Action() != domain.ActionUserDisable || got[0].Before()["enabled"] != "true" || got[0].Actor().Kind() != domain.ActorCLI {
		t.Errorf("newest entry = %+v", got[0])
	}

	e := got[1]
	if e.Actor().UserID() != u.ID() || e.Actor().IP().String() != "192.0.2.1" || e.TargetID() != u.ID().String() ||
		e.RequestID() != "req-1" || e.After()["provider"] != "local" || !e.At().Equal(t0) {
		t.Errorf("oldest entry = %+v", e)
	}
}
