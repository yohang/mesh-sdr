package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestUsername(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"ab", true}, {"Alice.B-c_9", true}, {strings.Repeat("a", 64), true},
		{"a", false}, {strings.Repeat("a", 65), false}, {"al ice", false}, {"al@ice", false}, {"élise", false}, {"", false},
	}

	for _, tt := range tests {
		u, err := domain.NewUsername(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("NewUsername(%q) err = %v", tt.in, err)
		}

		if err != nil && !errors.Is(err, domain.ErrInvalidUsername) {
			t.Errorf("NewUsername(%q) err = %v, want invalid_username", tt.in, err)
		}

		if tt.ok && u.String() != tt.in {
			t.Errorf("String() = %q", u.String())
		}
	}

	a, _ := domain.NewUsername("Alice")
	b, _ := domain.NewUsername("aLICE")

	if !a.Equal(b) || a.Key() != "alice" {
		t.Errorf("case-insensitive equality failed: %q %q", a.Key(), b.Key())
	}
}

func TestEmail(t *testing.T) {
	for in, ok := range map[string]bool{
		"a@example.org": true, "A.B+c@Example.ORG": true,
		"": false, "nope": false, "Alice <a@example.org>": false, " a@example.org": false,
		strings.Repeat("a", 250) + "@x.org": false,
	} {
		e, err := domain.NewEmail(in)
		if (err == nil) != ok {
			t.Errorf("NewEmail(%q) err = %v", in, err)
		}

		if ok && e.Key() != strings.ToLower(in) {
			t.Errorf("Key() = %q", e.Key())
		}
	}
}

func TestDisplayName(t *testing.T) {
	for in, ok := range map[string]bool{
		"Alice": true, "Élise Ø": true, strings.Repeat("é", 64): true,
		"": false, "   ": false, "a\nb": false, strings.Repeat("é", 65): false, "\xff": false,
	} {
		if _, err := domain.NewDisplayName(in); (err == nil) != ok {
			t.Errorf("NewDisplayName(%q) err = %v", in, err)
		}
	}
}

func TestLogin(t *testing.T) {
	l, err := domain.NewLogin("  Alice@Example.org ")
	if err != nil {
		t.Fatal(err)
	}

	if l.String() != "Alice@Example.org" || l.Key() != "alice@example.org" || !l.IsEmail() {
		t.Errorf("login = %q key %q email %v", l.String(), l.Key(), l.IsEmail())
	}

	if _, err := domain.NewLogin("  "); !errors.Is(err, domain.ErrInvalidLogin) {
		t.Errorf("blank login err = %v", err)
	}
}

func TestPassword(t *testing.T) {
	p := domain.DefaultPasswordPolicy()

	tests := []struct {
		in string
		ok bool
	}{
		{strings.Repeat("a", 10), true},
		{strings.Repeat("é", 10), true},
		{strings.Repeat("a", 9), false},
		{strings.Repeat("a", 256), true},
		{strings.Repeat("a", 257), false},
		{"\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8\xf7\xf6", false},
	}

	for _, tt := range tests {
		pw, err := domain.NewPassword(tt.in, p)
		if (err == nil) != tt.ok {
			t.Errorf("NewPassword(len %d) err = %v", len(tt.in), err)
		}

		if err == nil && (pw.Reveal() != tt.in || pw.String() != "<redacted>") {
			t.Error("password value or redaction wrong")
		}
	}

	if domain.NewPasswordPolicy(4).MinLength() != domain.PasswordMinLengthFloor {
		t.Error("policy below the floor accepted")
	}
}

func TestPasswordHash(t *testing.T) {
	if _, err := domain.NewPasswordHash("$argon2id$v=19$m=1,t=1,p=1$x$y"); err != nil {
		t.Error(err)
	}

	for _, in := range []string{"", "plain", "$" + strings.Repeat("x", 1024)} {
		if _, err := domain.NewPasswordHash(in); !errors.Is(err, domain.ErrInvalidHash) {
			t.Errorf("NewPasswordHash(%.10q) err = %v", in, err)
		}
	}
}

func TestIDs(t *testing.T) {
	b := []byte{0x01, 0x8f, 0x3a, 0x2b, 0x4c, 0x5d, 0x7e, 0x6f, 0x80, 0x91, 0xa2, 0xb3, 0xc4, 0xd5, 0xe6, 0xf7}

	id, err := domain.UserIDFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}

	const want = "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7"
	if id.String() != want {
		t.Errorf("String() = %s", id)
	}

	back, err := domain.ParseUserID(want)
	if err != nil || back != id {
		t.Errorf("ParseUserID = %v, %v", back, err)
	}

	for _, bad := range []string{"", "018f3a2b4c5d7e6f8091a2b3c4d5e6f7", "018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6fz", "00000000-0000-0000-0000-000000000000"} {
		if _, err := domain.ParseUserID(bad); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("ParseUserID(%q) err = %v", bad, err)
		}
	}

	if _, err := domain.SessionIDFromBytes(make([]byte, 16)); !errors.Is(err, domain.ErrInvalidID) {
		t.Error("nil session id accepted")
	}

	if _, err := domain.SessionIDFromBytes(b[:15]); !errors.Is(err, domain.ErrInvalidID) {
		t.Error("short session id accepted")
	}

	if _, err := domain.NewUserID(shared.UUID{}); !errors.Is(err, domain.ErrInvalidID) {
		t.Error("nil user id accepted")
	}
}

func TestRoles(t *testing.T) {
	for _, name := range []string{"anonymous", "listener", "operator", "admin"} {
		r, err := domain.ParseRole(name)
		if err != nil || r.String() != name {
			t.Errorf("ParseRole(%s) = %v, %v", name, r, err)
		}
	}

	if _, err := domain.ParseRole("root"); !errors.Is(err, domain.ErrInvalidRole) {
		t.Error("unknown role accepted")
	}

	if r, err := domain.RoleFromID(20); err != nil || r != domain.RoleOperator {
		t.Errorf("RoleFromID(20) = %v, %v", r, err)
	}

	if _, err := domain.RoleFromID(15); err == nil {
		t.Error("RoleFromID(15) accepted")
	}

	if !domain.RoleAdmin.Includes(domain.RoleOperator) || domain.RoleListener.Includes(domain.RoleOperator) {
		t.Error("rank order wrong")
	}

	dev, _ := domain.NewDeviceID("rtl-1")

	if _, err := domain.NewRoleGrant(domain.RoleOperator, dev); err != nil {
		t.Error(err)
	}

	if _, err := domain.NewRoleGrant(domain.RoleAdmin, dev); !errors.Is(err, domain.ErrInvalidRole) {
		t.Error("device-scoped admin accepted")
	}

	if _, err := domain.NewRoleGrant(domain.RoleListener, domain.DeviceID{}); !errors.Is(err, domain.ErrInvalidRole) {
		t.Error("listener grant accepted")
	}

	if _, err := domain.NewDeviceID("Bad.Id"); !errors.Is(err, domain.ErrInvalidDevice) {
		t.Error("bad device id accepted")
	}
}

func TestIdentity(t *testing.T) {
	if _, err := domain.NewProviderID("oidc:google"); err != nil {
		t.Error(err)
	}

	if _, err := domain.NewProviderID("OIDC"); err == nil {
		t.Error("uppercase provider accepted")
	}

	if _, err := domain.NewIdentity(domain.ProviderLocal, ""); !errors.Is(err, domain.ErrInvalidIdentity) {
		t.Error("empty subject accepted")
	}

	id, _ := domain.ParseUserID("018f3a2b-4c5d-7e6f-8091-a2b3c4d5e6f7")
	li := domain.LocalIdentity(id)

	if li.Provider() != domain.ProviderLocal || li.Subject() != id.String() {
		t.Errorf("local identity = %v %v", li.Provider(), li.Subject())
	}
}
