package domain_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func mustUserID(t *testing.T, s string) domain.UserID {
	t.Helper()

	id, err := domain.ParseUserID(s)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func newUser(t *testing.T, grants ...domain.RoleGrant) *domain.User {
	t.Helper()

	name, _ := domain.NewUsername("alice")
	hash, _ := domain.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA")

	u, err := domain.NewLocalUser(domain.NewLocalUserParams{
		ID: mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7"), Username: name, PasswordHash: hash,
		Grants: grants, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	return u
}

func TestNewLocalUser(t *testing.T) {
	u := newUser(t)

	if !u.Enabled() || u.Version() != 1 || u.Origin() != domain.OriginDB || !u.CanPasswordLogin() {
		t.Errorf("new user = enabled %v version %d origin %s can login %v", u.Enabled(), u.Version(), u.Origin(), u.CanPasswordLogin())
	}

	ids := u.Identities()
	if len(ids) != 1 || ids[0] != domain.LocalIdentity(u.ID()) {
		t.Errorf("identities = %v", ids)
	}

	if u.Role() != domain.RoleListener {
		t.Errorf("role = %v", u.Role())
	}

	if _, err := domain.NewLocalUser(domain.NewLocalUserParams{ID: u.ID(), Username: u.Username(), Now: t0}); !errors.Is(err, domain.ErrInvalidUser) {
		t.Errorf("user without password: %v", err)
	}

	admin, _ := domain.NewRoleGrant(domain.RoleAdmin, shared.DeviceID{})
	if r := newUser(t, admin, admin).Role(); r != domain.RoleAdmin {
		t.Errorf("admin role = %v", r)
	}
}

func TestRehydrateUserWithoutLocalPassword(t *testing.T) {
	name, _ := domain.NewUsername("bob")
	id := mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f8")
	oidc, _ := domain.NewProviderID("oidc:test")
	ident, _ := domain.NewIdentity(oidc, "sub-1")

	u, err := domain.RehydrateUser(domain.UserState{
		ID: id, Username: name, Enabled: true, Origin: domain.OriginDB, Version: 3,
		CreatedAt: t0, UpdatedAt: t0, Identities: []domain.Identity{ident},
	})
	if err != nil {
		t.Fatal(err)
	}

	if u.CanPasswordLogin() || u.HasLocalIdentity() {
		t.Error("an OIDC-only user can log in with a password")
	}

	if _, err := domain.RehydrateUser(domain.UserState{ID: id, Username: name, Origin: "nope", Version: 1}); err == nil {
		t.Error("bad origin accepted")
	}
}

func TestLoginThrottling(t *testing.T) {
	u := newUser(t)
	p := domain.DefaultThrottlePolicy()
	now := t0

	for i := 1; i <= 4; i++ {
		if u.RecordLoginFailure(now, p) {
			t.Fatalf("locked after %d failures", i)
		}

		if blocked, _ := u.BlockedAt(now); blocked {
			t.Fatalf("delayed after %d failures", i)
		}
	}

	u.RecordLoginFailure(now, p) // 5th: 1 s delay
	if blocked, until := u.BlockedAt(now); !blocked || until.Sub(now) != time.Second {
		t.Fatalf("5th failure: blocked %v until %v", blocked, until.Sub(now))
	}

	for range 4 {
		u.RecordLoginFailure(now, p)
	}

	if _, until := u.BlockedAt(now); until.Sub(now) != 16*time.Second {
		t.Errorf("9th failure delay = %v", until.Sub(now))
	}

	if !u.RecordLoginFailure(now, p) {
		t.Error("10th failure did not lock")
	}

	if _, until := u.BlockedAt(now); until.Sub(now) != 15*time.Minute {
		t.Errorf("lock = %v", until.Sub(now))
	}

	u.RecordLoginSuccess(now)

	if u.FailedLogins() != 0 || !u.LockedUntil().IsZero() || !u.LastLoginAt().Equal(now) {
		t.Errorf("after success: %d %v %v", u.FailedLogins(), u.LockedUntil(), u.LastLoginAt())
	}
}

func TestThrottlePolicy(t *testing.T) {
	p := domain.DefaultThrottlePolicy()

	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0}, {4, 0}, {5, time.Second}, {6, 2 * time.Second}, {9, 16 * time.Second},
		{10, 15 * time.Minute}, {11, 30 * time.Minute}, {12, time.Hour}, {30, 24 * time.Hour},
	}

	for _, tt := range tests {
		until := p.BlockedUntil(tt.failures, t0)

		got := time.Duration(0)
		if !until.IsZero() {
			got = until.Sub(t0)
		}

		if got != tt.want {
			t.Errorf("BlockedUntil(%d) = %v, want %v", tt.failures, got, tt.want)
		}
	}

	custom := domain.NewThrottlePolicy(2, 3, time.Minute, time.Hour)
	if custom.BlockedUntil(3, t0).Sub(t0) != time.Minute || !custom.Locks(3) || custom.Locks(2) {
		t.Error("custom policy wrong")
	}
}

func TestDisableEnable(t *testing.T) {
	u := newUser(t)
	p := domain.DefaultThrottlePolicy()

	for range 10 {
		u.RecordLoginFailure(t0, p)
	}

	if !u.Disable(t0.Add(time.Hour)) || u.Enabled() || u.Disable(t0) {
		t.Error("disable is not idempotent")
	}

	if !u.UpdatedAt().Equal(t0.Add(time.Hour)) {
		t.Errorf("updated_at = %v", u.UpdatedAt())
	}

	if !u.Enable(t0) || !u.Enabled() || u.Enable(t0) {
		t.Error("enable is not idempotent")
	}

	if u.FailedLogins() != 0 || !u.LockedUntil().IsZero() {
		t.Error("enable keeps the lock-out")
	}

	u.Saved()

	if u.Version() != 2 {
		t.Errorf("version = %d", u.Version())
	}
}

func TestPrincipal(t *testing.T) {
	anon := domain.Anonymous()

	if !anon.IsAnonymous() || anon.Role() != domain.RoleAnonymous || anon.Roles() != nil {
		t.Error("anonymous principal wrong")
	}

	if !anon.CanListen(domain.ListenAnonymous) || anon.CanListen(domain.ListenRegistered) || anon.CanListen("") {
		t.Error("anonymous listen policy wrong")
	}

	dev, _ := shared.NewDeviceID("rtl-1")
	other, _ := shared.NewDeviceID("rtl-2")
	op, _ := domain.NewRoleGrant(domain.RoleOperator, dev)
	u := newUser(t, op)

	s, _, err := domain.StartSession(domain.StartSessionParams{
		ID: mustSessionID(t), UserID: u.ID(), Provider: domain.ProviderLocal, Now: t0, IP: netip.MustParseAddr("::ffff:10.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	p := domain.UserPrincipal(u, s)

	if p.IsAnonymous() || p.Role() != domain.RoleListener || p.Has(domain.RoleOperator) {
		t.Errorf("scoped operator: role %v", p.Role())
	}

	if !p.HasOnDevice(domain.RoleOperator, dev) || p.HasOnDevice(domain.RoleOperator, other) {
		t.Error("device scope wrong")
	}

	if got := p.Roles(); len(got) != 2 || got[0] != domain.RoleListener || got[1] != domain.RoleOperator {
		t.Errorf("roles = %v", got)
	}

	if !p.CanListen(domain.ListenRegistered) {
		t.Error("user cannot listen to a registered device")
	}

	if p.SessionID() != s.ID() || p.UserID() != u.ID() {
		t.Error("ids wrong")
	}

	if s.IP().String() != "10.0.0.1" {
		t.Errorf("session ip not canonical: %s", s.IP())
	}
}

func TestChangeAndResetPassword(t *testing.T) {
	h, _ := domain.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3$bmV3")
	p := domain.DefaultThrottlePolicy()

	u := newUser(t)
	for range 10 {
		u.RecordLoginFailure(t0, p)
	}

	if err := u.ResetPassword(h, true, t0); err != nil {
		t.Fatal(err)
	}

	if u.PasswordHash() != h || !u.MustChangePassword() || u.FailedLogins() != 0 || !u.LockedUntil().IsZero() {
		t.Errorf("after reset: flag %v, failures %d, locked %v", u.MustChangePassword(), u.FailedLogins(), u.LockedUntil())
	}

	if err := u.ChangePassword(h, t0); err != nil || u.MustChangePassword() {
		t.Errorf("change: %v, flag %v", err, u.MustChangePassword())
	}

	if err := u.ChangePassword(domain.PasswordHash{}, t0); !errors.Is(err, domain.ErrInvalidHash) {
		t.Errorf("empty hash: %v", err)
	}

	if err := u.ResetPassword(domain.PasswordHash{}, false, t0); !errors.Is(err, domain.ErrInvalidHash) {
		t.Errorf("empty hash: %v", err)
	}
}
