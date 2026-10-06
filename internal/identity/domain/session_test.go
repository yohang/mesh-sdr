package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func mustSessionID(t *testing.T) domain.SessionID {
	t.Helper()

	id, err := domain.SessionIDFromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func startSession(t *testing.T, remember bool) (*domain.Session, domain.SessionToken) {
	t.Helper()

	s, tok, err := domain.StartSession(domain.StartSessionParams{
		ID: mustSessionID(t), UserID: mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7"),
		Provider: domain.ProviderLocal, Remember: remember, Now: t0, UserAgent: strings.Repeat("é", 300),
		Policy: domain.DefaultSessionPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return s, tok
}

func TestSessionToken(t *testing.T) {
	a, b := domain.NewSessionToken(), domain.NewSessionToken()
	if a == b {
		t.Fatal("two tokens are equal")
	}

	if len(a.Cookie()) != 43 || a.String() != "<redacted>" {
		t.Errorf("cookie %q string %q", a.Cookie(), a.String())
	}

	back, err := domain.ParseSessionToken(a.Cookie())
	if err != nil || back != a || back.Hash() != a.Hash() {
		t.Errorf("round trip failed: %v", err)
	}

	for _, bad := range []string{"", "abc", a.Cookie() + "A", strings.Repeat("*", 43)} {
		if _, err := domain.ParseSessionToken(bad); !errors.Is(err, domain.ErrInvalidToken) {
			t.Errorf("ParseSessionToken(%q) err = %v", bad, err)
		}
	}
}

func TestStartSession(t *testing.T) {
	s, tok := startSession(t, false)

	if s.TokenHash() != tok.Hash() {
		t.Error("stored hash does not match the token")
	}

	if s.AbsoluteExpiresAt().Sub(t0) != 24*time.Hour || s.IdleExpiresAt().Sub(t0) != 24*time.Hour {
		t.Errorf("expiry = %v / %v", s.IdleExpiresAt(), s.AbsoluteExpiresAt())
	}

	if len(s.UserAgent()) > 512 || !strings.HasPrefix(s.UserAgent(), "é") {
		t.Errorf("user agent not truncated on a rune boundary: %d bytes", len(s.UserAgent()))
	}

	long, _ := startSession(t, true)
	if long.AbsoluteExpiresAt().Sub(t0) != 30*24*time.Hour || long.IdleExpiresAt().Sub(t0) != 24*time.Hour {
		t.Errorf("remember-me expiry = %v / %v", long.IdleExpiresAt(), long.AbsoluteExpiresAt())
	}

	if _, _, err := domain.StartSession(domain.StartSessionParams{Now: t0}); !errors.Is(err, domain.ErrInvalidSession) {
		t.Errorf("empty params: %v", err)
	}
}

func TestSessionLifetime(t *testing.T) {
	p := domain.DefaultSessionPolicy()
	s, _ := startSession(t, true)

	if !s.ActiveAt(t0) || s.Touch(t0.Add(30*time.Second), p) {
		t.Error("touch within a minute must not write")
	}

	if !s.Touch(t0.Add(20*time.Hour), p) || !s.IdleExpiresAt().Equal(t0.Add(44*time.Hour)) {
		t.Errorf("touch did not extend idle expiry: %v", s.IdleExpiresAt())
	}

	if !s.ActiveAt(t0.Add(43*time.Hour)) || s.ActiveAt(t0.Add(44*time.Hour)) {
		t.Error("idle expiry wrong")
	}

	if s.Touch(t0.Add(45*time.Hour), p) {
		t.Error("an idle-expired session was touched")
	}

	short, _ := startSession(t, false)
	for h := 1; h < 30; h++ {
		short.Touch(t0.Add(time.Duration(h)*time.Hour), p)
	}

	if short.ActiveAt(t0.Add(24 * time.Hour)) {
		t.Error("absolute expiry not enforced")
	}

	if !short.IdleExpiresAt().Equal(short.AbsoluteExpiresAt()) {
		t.Error("idle expiry beyond the absolute expiry")
	}
}

func TestSessionRevoke(t *testing.T) {
	s, _ := startSession(t, false)

	if !s.Revoke(domain.RevokeLogout, t0.Add(time.Minute)) || s.ActiveAt(t0.Add(2*time.Minute)) {
		t.Error("revoke failed")
	}

	if s.Revoke(domain.RevokeAdmin, t0.Add(time.Hour)) || s.RevokeReason() != domain.RevokeLogout {
		t.Error("second revoke changed the reason")
	}

	if _, err := domain.ParseRevokeReason("whatever"); err == nil {
		t.Error("bad reason accepted")
	}
}

func TestCSRF(t *testing.T) {
	s, tok := startSession(t, false)
	other := domain.NewSessionToken()
	csrf := s.CSRFSecret().Token(tok)

	if !s.CSRFSecret().Verify(tok, csrf) {
		t.Error("valid token rejected")
	}

	if s.CSRFSecret().Verify(other, csrf) || s.CSRFSecret().Verify(tok, "") || s.CSRFSecret().Verify(tok, csrf+"x") {
		t.Error("invalid token accepted")
	}

	again, err := domain.RehydrateCSRFSecret(s.CSRFSecret().Bytes())
	if err != nil || again.Token(tok) != csrf {
		t.Error("rehydrated secret differs")
	}

	if domain.NewCSRFSecret().Token(tok) == csrf {
		t.Error("two secrets give the same token")
	}
}

func TestRateLimitError(t *testing.T) {
	err := domain.NewRateLimitError(1500 * time.Millisecond)

	if err.RetryAfter() != 2*time.Second || !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("retry after %v, is rate limited %v", err.RetryAfter(), errors.Is(err, domain.ErrRateLimited))
	}

	if domain.NewRateLimitError(0).RetryAfter() != time.Second {
		t.Error("minimum wait is not one second")
	}
}

func TestAuditEntry(t *testing.T) {
	e, err := domain.NewAuditEntry(t0, domain.CLIActor(), domain.ActionUserDisable, domain.ResultOK)
	if err != nil {
		t.Fatal(err)
	}

	e = e.WithTarget("user", strings.Repeat("x", 200)).WithAfter(map[string]string{"enabled": "false"})

	if len(e.TargetID()) != 128 || e.After()["enabled"] != "false" || e.Actor().Kind() != domain.ActorCLI {
		t.Error("entry fields wrong")
	}

	for _, bad := range []string{"", "login", "Auth.Login", "auth..x"} {
		if _, err := domain.NewAuditEntry(t0, domain.CLIActor(), bad, domain.ResultOK); err == nil {
			t.Errorf("action %q accepted", bad)
		}
	}

	if _, err := domain.NewAuditEntry(t0, domain.Actor{}, domain.ActionLogout, domain.ResultOK); err == nil {
		t.Error("zero actor accepted")
	}

	if _, err := domain.NewAuditEntry(t0, domain.SystemActor(), domain.ActionLogout, "maybe"); err == nil {
		t.Error("bad result accepted")
	}
}

func TestSessionPersistentAndNotAfter(t *testing.T) {
	p := domain.DefaultSessionPolicy()

	plain, _ := startSession(t, false)
	remembered, _ := startSession(t, true)

	if plain.Persistent(p) || !remembered.Persistent(p) {
		t.Errorf("persistent: plain %v, remembered %v", plain.Persistent(p), remembered.Persistent(p))
	}

	capped, _, err := domain.StartSession(domain.StartSessionParams{
		ID: mustSessionID(t), UserID: plain.UserID(), Provider: domain.ProviderLocal, Remember: true,
		Policy: p, Now: t0.Add(time.Hour), NotAfter: remembered.AbsoluteExpiresAt(),
	})
	if err != nil || !capped.AbsoluteExpiresAt().Equal(remembered.AbsoluteExpiresAt()) {
		t.Fatalf("capped session: %v, expires %v", err, capped.AbsoluteExpiresAt())
	}

	if _, _, err := domain.StartSession(domain.StartSessionParams{
		ID: mustSessionID(t), UserID: plain.UserID(), Provider: domain.ProviderLocal, Policy: p, Now: t0, NotAfter: t0,
	}); !errors.Is(err, domain.ErrInvalidSession) {
		t.Errorf("already expired session: %v", err)
	}
}
