package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()

	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return k
}

func claims() token.Claims {
	return token.Claims{
		Issuer: "https://sdr.example.org", Audience: token.Audience("roof"), Subject: "u1", SessionID: "abcd",
		ConnectionID: "c1", Roles: []string{"listener"},
		Scopes:   []token.Scope{{Device: "hf", Perms: []string{token.PermListen, token.PermDemod}}},
		Limits:   token.Limits{MaxDemods: 2},
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(5 * time.Minute), ID: "j1",
	}
}

func keySet(t *testing.T, revoked []string, keys ...ed25519.PrivateKey) *token.KeySet {
	t.Helper()

	var jwks token.JWKS
	for _, k := range keys {
		jwks.Keys = append(jwks.Keys, token.NewJWK(k.Public().(ed25519.PublicKey)))
	}

	// Round trip through JSON, as over ctl.keys.update.
	b, err := json.Marshal(jwks)
	if err != nil {
		t.Fatal(err)
	}

	var back token.JWKS
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}

	s, err := token.NewKeySet(back, revoked)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func expect(at time.Time) token.Expect {
	return token.Expect{Issuer: "https://sdr.example.org", Audience: token.Audience("roof"), Now: at}
}

func TestSignVerify(t *testing.T) {
	k := newKey(t)

	raw, err := token.Sign(claims(), k)
	if err != nil {
		t.Fatal(err)
	}

	got, err := token.Verify(raw, keySet(t, nil, newKey(t), k), expect(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}

	if got.ConnectionID != "c1" || got.Subject != "u1" || got.SessionID != "abcd" || got.Audience != token.Audience("roof") ||
		!got.ExpiresAt.Equal(now.Add(5*time.Minute)) || got.Limits.MaxDemods != 2 {
		t.Fatalf("claims = %+v", got)
	}

	if !got.Allows("hf", token.PermDemod) || got.Allows("hf", token.PermRetune) || got.Allows("vhf", token.PermListen) {
		t.Fatal("scope checks")
	}
}

func TestVerifyFailures(t *testing.T) {
	k := newKey(t)
	keys := keySet(t, nil, k)

	sign := func(mut func(*token.Claims)) string {
		c := claims()
		mut(&c)

		raw, err := token.Sign(c, k)
		if err != nil {
			t.Fatal(err)
		}

		return raw
	}

	valid := sign(func(*token.Claims) {})

	cases := []struct {
		name string
		raw  string
		keys *token.KeySet
		at   time.Time
		want error
	}{
		{"expired", valid, keys, now.Add(5*time.Minute + token.Leeway + time.Second), token.ErrExpired},
		{"within leeway", valid, keys, now.Add(5*time.Minute + token.Leeway - time.Second), nil},
		{"not yet valid", valid, keys, now.Add(-token.Leeway - time.Second), token.ErrInvalid},
		{"wrong audience", sign(func(c *token.Claims) { c.Audience = token.Audience("shack") }), keys, now, token.ErrInvalid},
		{"wrong issuer", sign(func(c *token.Claims) { c.Issuer = "https://evil" }), keys, now, token.ErrInvalid},
		{"no cid", sign(func(c *token.Claims) { c.ConnectionID = "" }), keys, now, token.ErrInvalid},
		{"no exp", sign(func(c *token.Claims) { c.ExpiresAt = time.Time{} }), keys, now, token.ErrInvalid},
		{"unknown key", valid, keySet(t, nil, newKey(t)), now, token.ErrUnknownKey},
		{"revoked key", valid, keySet(t, []string{token.Thumbprint(k.Public().(ed25519.PublicKey))}, k), now, token.ErrUnknownKey},
		{"no key set", valid, nil, now, token.ErrUnknownKey},
		{"garbage", "a.b.c", keys, now, token.ErrInvalid},
		{"tampered", tamper(valid), keys, now, token.ErrInvalid},
		{"alg none", noneToken(), keys, now, token.ErrInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := token.Verify(tc.raw, tc.keys, expect(tc.at))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}

				return
			}

			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func tamper(raw string) string {
	parts := strings.Split(raw, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), `"listener"`, `"admin"`, 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)

	return strings.Join(parts, ".")
}

func noneToken() string {
	enc := base64.RawURLEncoding.EncodeToString

	return enc([]byte(`{"alg":"none","kid":"x"}`)) + "." +
		enc([]byte(`{"iss":"https://sdr.example.org","aud":["rx-node:roof"],"cid":"c1","exp":9999999999}`)) + "."
}

func TestJWK(t *testing.T) {
	k := newKey(t)
	jwk := token.NewJWK(k.Public().(ed25519.PublicKey))

	if _, err := jwk.PublicKey(); err != nil {
		t.Fatal(err)
	}

	bad := jwk
	bad.Kid = "other"

	if _, err := bad.PublicKey(); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("kid mismatch: err = %v", err)
	}

	if _, err := token.NewKeySet(token.JWKS{Keys: []token.JWK{bad}}, nil); err == nil {
		t.Fatal("key set accepted a bad key")
	}
}

// RFC 8037 Appendix A.3: thumbprint of the example Ed25519 key.
func TestThumbprintRFC8037(t *testing.T) {
	x, err := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if err != nil {
		t.Fatal(err)
	}

	if got := token.Thumbprint(x); got != "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k" {
		t.Fatalf("thumbprint = %s", got)
	}
}
