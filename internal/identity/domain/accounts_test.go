package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var idGen = shared.NewUUIDv7Generator()

func newInvitationID(t *testing.T) domain.InvitationID {
	t.Helper()

	u, _ := idGen.New(t0)
	id, err := domain.NewInvitationID(u)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func newTokenID(t *testing.T) domain.TokenID {
	t.Helper()

	u, _ := idGen.New(t0)
	id, err := domain.NewTokenID(u)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func TestLinkToken(t *testing.T) {
	tok := domain.NewLinkToken()

	parsed, err := domain.ParseLinkToken(tok.Text())
	if err != nil || parsed.Hash() != tok.Hash() || tok.String() != "<redacted>" || len(tok.Text()) != 43 {
		t.Fatalf("round trip: %v", err)
	}

	for _, bad := range []string{"", "short", tok.Text() + "x", "!!!"} {
		if _, err := domain.ParseLinkToken(bad); !errors.Is(err, domain.ErrInvalidToken) {
			t.Errorf("ParseLinkToken(%q) = %v", bad, err)
		}
	}
}

func TestInvitation(t *testing.T) {
	dev, _ := domain.NewDeviceID("rtl-1")
	mail, _ := domain.NewEmail("x@example.org")
	ok := domain.NewInvitationParams{ID: newInvitationID(t), Role: domain.RoleOperator, Device: dev, Delivery: domain.DeliveryLink, Now: t0, TTL: time.Hour}

	for name, p := range map[string]domain.NewInvitationParams{
		"admin with device": {ID: ok.ID, Role: domain.RoleAdmin, Device: dev, Delivery: domain.DeliveryLink, Now: t0, TTL: time.Hour},
		"too long":          {ID: ok.ID, Role: domain.RoleListener, Delivery: domain.DeliveryLink, Now: t0, TTL: 31 * 24 * time.Hour},
		"e-mail without":    {ID: ok.ID, Role: domain.RoleListener, Delivery: domain.DeliveryEmail, Now: t0, TTL: time.Hour},
		"anonymous":         {ID: ok.ID, Role: domain.RoleAnonymous, Delivery: domain.DeliveryLink, Now: t0, TTL: time.Hour},
		"no id":             {Role: domain.RoleListener, Delivery: domain.DeliveryLink, Now: t0, TTL: time.Hour},
	} {
		if _, _, err := domain.NewInvitation(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	inv, _, err := domain.NewInvitation(ok)
	if err != nil {
		t.Fatal(err)
	}

	if g := inv.Grants(); len(g) != 1 || g[0].Role() != domain.RoleOperator || g[0].Device() != dev {
		t.Errorf("grants = %v", g)
	}

	if inv.StateAt(t0.Add(time.Hour)) != domain.InvitationExpired || inv.StateAt(t0) != domain.InvitationPending {
		t.Error("state")
	}

	if err := inv.Redeem(mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7"), t0.Add(time.Hour)); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("redeem after expiry: %v", err)
	}

	if err := inv.Revoke(t0); err != nil || inv.StateAt(t0) != domain.InvitationRevoked {
		t.Errorf("revoke: %v", err)
	}

	if err := inv.Revoke(t0); !errors.Is(err, domain.ErrInvitationNotPending) {
		t.Errorf("revoke twice: %v", err)
	}

	listener, _, _ := domain.NewInvitation(domain.NewInvitationParams{ID: newInvitationID(t), Role: domain.RoleListener, Email: mail, Delivery: domain.DeliveryEmail, Now: t0, TTL: time.Hour})
	if len(listener.Grants()) != 0 {
		t.Error("listener has a grant row")
	}

	uid := mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7")
	if err := listener.Redeem(uid, t0); err != nil || listener.StateAt(t0) != domain.InvitationRedeemed || listener.RedeemedUserID() != uid {
		t.Errorf("redeem: %v", err)
	}

	if err := listener.Redeem(uid, t0); !errors.Is(err, domain.ErrInvitationInvalid) {
		t.Errorf("redeem twice: %v", err)
	}
}

func TestOneTimeTokens(t *testing.T) {
	uid := mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7")

	if _, _, err := domain.NewPasswordResetToken(newTokenID(t), uid, "", t0, 25*time.Hour); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("too long: %v", err)
	}

	r, _, err := domain.NewPasswordResetToken(newTokenID(t), uid, "192.0.2.1", t0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Use(t0.Add(30 * time.Minute)); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("expired use: %v", err)
	}

	if err := r.Use(t0); err != nil || r.ValidAt(t0) {
		t.Errorf("use: %v", err)
	}

	if err := r.Use(t0); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("second use: %v", err)
	}

	mail, _ := domain.NewEmail("next@example.org")

	e, _, err := domain.NewEmailChangeToken(newTokenID(t), uid, mail, t0)
	if err != nil || e.NewEmail() != mail || !e.ExpiresAt().Equal(t0.Add(domain.EmailChangeTTL)) {
		t.Errorf("e-mail token: %v", err)
	}

	if _, _, err := domain.NewEmailChangeToken(newTokenID(t), uid, domain.Email{}, t0); !errors.Is(err, domain.ErrInvalidEmail) {
		t.Errorf("no address: %v", err)
	}
}

func TestUserProfileAndGrants(t *testing.T) {
	u := newUser(t)
	op, _ := domain.NewRoleGrant(domain.RoleOperator, domain.DeviceID{})
	listener, _ := domain.NewRoleGrant(domain.RoleListener, domain.DeviceID{})
	by := mustUserID(t, "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f8")

	if !u.ReplaceGrants([]domain.RoleGrant{op, listener, op}, by, t0) || len(u.Grants()) != 1 || u.Role() != domain.RoleOperator {
		t.Errorf("grants = %v", u.Grants())
	}

	if gotBy, at := u.GrantedBy(); gotBy != by || !at.Equal(t0) {
		t.Errorf("granted by = %v %v", gotBy, at)
	}

	if u.ReplaceGrants([]domain.RoleGrant{op}, by, t0) {
		t.Error("same grants reported as a change")
	}

	mail, _ := domain.NewEmail("a@example.org")
	if !u.SetEmail(mail, false, t0) || !u.EmailVerifiedAt().IsZero() {
		t.Error("unverified e-mail")
	}

	if !u.SetEmail(mail, true, t0) || u.EmailVerifiedAt().IsZero() {
		t.Error("verification not recorded")
	}

	other, _ := domain.NewEmail("b@example.org")
	if !u.SetEmail(other, false, t0) || !u.EmailVerifiedAt().IsZero() {
		t.Error("a new address kept the verification")
	}

	h, _ := domain.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3$bmV3")
	if err := u.CompleteReset(h, t0); err != nil || u.EmailVerifiedAt().IsZero() || u.MustChangePassword() {
		t.Errorf("complete reset: %v", err)
	}

	name, _ := domain.NewDisplayName("Alice")
	if !u.SetDisplayName(name, t0) || u.SetDisplayName(name, t0) || u.DisplayName() != name {
		t.Error("display name")
	}
}
