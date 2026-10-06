// Package token is the access-token contract between the hub, which issues
// tokens, and the nodes, which verify them offline (TECHNICAL_SPEC §5.8).
//
// An access token is a compact JWS signed with Ed25519 (alg EdDSA, RFC 8037).
// Its header carries the kid of the signing key: the RFC 7638 thumbprint of
// the public key. The public keys reach the nodes as a JWKS over the control
// channel (ctl.keys.update).
//
// The package is neutral: the hub signer (identity) and the node verifier
// (grid) both use it, so the claim names and checks are defined once. It is
// pure: no I/O, no logging, no clock (callers pass the time).
package token

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Algorithm is the only accepted JWS algorithm.
const Algorithm = "EdDSA"

// Leeway is the clock-skew tolerance applied to nbf and exp (§5.8).
const Leeway = 30 * time.Second

// AnonymousSubject is the subject of an anonymous listener.
const AnonymousSubject = "anon"

// Permissions carried in Scope.Perms (§5.8).
const (
	PermListen = "listen"
	PermDemod  = "demod"
	PermPreset = "preset"
	PermRetune = "retune"
)

// Verification errors. ErrExpired is returned alone when the token is valid
// but past exp + Leeway, so that callers can close with 4401 token_expired.
var (
	ErrInvalid    = errors.New("token_invalid")
	ErrExpired    = errors.New("token_expired")
	ErrUnknownKey = errors.New("token_unknown_key")
)

// Audience returns the audience of the tokens of node nodeID: rx-node:<id>.
func Audience(nodeID string) string { return "rx-node:" + nodeID }

// Scope is one device entry of the scp claim.
type Scope struct {
	Device string   `json:"dev"`
	Perms  []string `json:"perm"`
}

// Allows reports whether the scope grants perm.
func (s Scope) Allows(perm string) bool { return slices.Contains(s.Perms, perm) }

// Limits is the lim claim.
type Limits struct {
	MaxDemods int `json:"max_demods"`
}

// Claims are the claims of an access token (§5.8).
type Claims struct {
	Issuer       string // iss: hub.url
	Audience     string // aud: Audience(nodeID)
	Subject      string // sub: user id or AnonymousSubject
	SessionID    string // sid: session id hash prefix, empty for anonymous
	ConnectionID string // cid: one per media WebSocket, issued by the hub
	Roles        []string
	Scopes       []Scope
	Limits       Limits
	IssuedAt     time.Time
	NotBefore    time.Time
	ExpiresAt    time.Time
	ID           string // jti
}

// Scope returns the scope of device, if any.
func (c Claims) Scope(device string) (Scope, bool) {
	for _, s := range c.Scopes {
		if s.Device == device {
			return s, true
		}
	}

	return Scope{}, false
}

// Allows reports whether the token grants perm on device.
func (c Claims) Allows(device, perm string) bool {
	s, ok := c.Scope(device)

	return ok && s.Allows(perm)
}

// wireClaims is the JSON form of Claims.
type wireClaims struct {
	jwt.RegisteredClaims

	SessionID    string   `json:"sid,omitempty"`
	ConnectionID string   `json:"cid"`
	Roles        []string `json:"roles"`
	Scopes       []Scope  `json:"scp"`
	Limits       Limits   `json:"lim"`
}

func numeric(t time.Time) *jwt.NumericDate {
	if t.IsZero() {
		return nil
	}

	return jwt.NewNumericDate(t)
}

func timeOf(d *jwt.NumericDate) time.Time {
	if d == nil {
		return time.Time{}
	}

	return d.Time
}

// Sign returns the compact JWS of c signed by key, with header kid
// Thumbprint(key.Public()).
func Sign(c Claims, key ed25519.PrivateKey) (string, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("%w: not an Ed25519 key", ErrInvalid)
	}

	w := wireClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    c.Issuer,
			Subject:   c.Subject,
			Audience:  jwt.ClaimStrings{c.Audience},
			ExpiresAt: numeric(c.ExpiresAt),
			NotBefore: numeric(c.NotBefore),
			IssuedAt:  numeric(c.IssuedAt),
			ID:        c.ID,
		},
		SessionID: c.SessionID, ConnectionID: c.ConnectionID,
		Roles: nonNil(c.Roles), Scopes: nonNil(c.Scopes), Limits: c.Limits,
	}

	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, w)
	t.Header["kid"] = Thumbprint(pub)

	s, err := t.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}

	return s, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}

	return s
}

// Expect are the values a verifier requires.
type Expect struct {
	Issuer   string    // required iss
	Audience string    // required aud
	Now      time.Time // verification time
}

// Verify checks raw against keys and expect: algorithm, kid, signature, iss,
// aud, nbf and exp (with Leeway), and the presence of cid and exp. It
// returns ErrUnknownKey for a kid missing from keys (or revoked), ErrExpired
// for an expired token and ErrInvalid otherwise.
func Verify(raw string, keys *KeySet, expect Expect) (Claims, error) {
	var w wireClaims

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{Algorithm}),
		jwt.WithIssuer(expect.Issuer),
		jwt.WithAudience(expect.Audience),
		jwt.WithLeeway(Leeway),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return expect.Now }),
	)

	_, err := parser.ParseWithClaims(raw, &w, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)

		pub, ok := keys.Key(kid)
		if !ok {
			return nil, ErrUnknownKey
		}

		return pub, nil
	})

	switch {
	case err == nil:
	case errors.Is(err, ErrUnknownKey):
		return Claims{}, ErrUnknownKey
	case errors.Is(err, jwt.ErrTokenExpired) && onlyExpired(err):
		return Claims{}, ErrExpired
	default:
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if w.ConnectionID == "" {
		return Claims{}, fmt.Errorf("%w: no cid", ErrInvalid)
	}

	aud := ""
	if len(w.Audience) > 0 {
		aud = w.Audience[0]
	}

	return Claims{
		Issuer: w.Issuer, Audience: aud, Subject: w.Subject, SessionID: w.SessionID, ConnectionID: w.ConnectionID,
		Roles: w.Roles, Scopes: w.Scopes, Limits: w.Limits,
		IssuedAt: timeOf(w.IssuedAt), NotBefore: timeOf(w.NotBefore), ExpiresAt: timeOf(w.ExpiresAt), ID: w.ID,
	}, nil
}

// onlyExpired reports whether the expiry is the only validation failure
// (golang-jwt joins every failed check).
func onlyExpired(err error) bool {
	for _, other := range []error{
		jwt.ErrTokenMalformed, jwt.ErrTokenUnverifiable, jwt.ErrTokenSignatureInvalid,
		jwt.ErrTokenInvalidAudience, jwt.ErrTokenInvalidIssuer, jwt.ErrTokenNotValidYet,
		jwt.ErrTokenUsedBeforeIssued, jwt.ErrTokenRequiredClaimMissing,
	} {
		if errors.Is(err, other) {
			return false
		}
	}

	return true
}

// JWK is an Ed25519 public key in JWK form (RFC 8037).
type JWK struct {
	Kty string `json:"kty"` // OKP
	Crv string `json:"crv"` // Ed25519
	X   string `json:"x"`   // base64url public key
	Kid string `json:"kid"` // Thumbprint
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// NewJWK returns the JWK of pub.
func NewJWK(pub ed25519.PublicKey) JWK {
	return JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: Thumbprint(pub), Use: "sig", Alg: Algorithm}
}

// PublicKey decodes the key and checks its kid.
func (k JWK) PublicKey() (ed25519.PublicKey, error) {
	if k.Kty != "OKP" || k.Crv != "Ed25519" {
		return nil, fmt.Errorf("%w: unsupported key %s/%s", ErrInvalid, k.Kty, k.Crv)
	}

	b, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: malformed Ed25519 key", ErrInvalid)
	}

	pub := ed25519.PublicKey(b)
	if k.Kid != Thumbprint(pub) {
		return nil, fmt.Errorf("%w: kid is not the key thumbprint", ErrInvalid)
	}

	return pub, nil
}

// Thumbprint returns the RFC 7638 thumbprint of pub, base64url without
// padding: the kid of the key.
func Thumbprint(pub ed25519.PublicKey) string {
	// Members in lexical order, no whitespace (RFC 7638 §3).
	doc, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
	}{"Ed25519", "OKP", base64.RawURLEncoding.EncodeToString(pub)})
	sum := sha256.Sum256(doc)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// KeySet is an immutable set of verification keys minus revoked kids.
type KeySet struct {
	keys map[string]ed25519.PublicKey
}

// NewKeySet builds the key set of jwks, without the revoked kids.
func NewKeySet(jwks JWKS, revokedKids []string) (*KeySet, error) {
	s := &KeySet{keys: make(map[string]ed25519.PublicKey, len(jwks.Keys))}

	for _, k := range jwks.Keys {
		pub, err := k.PublicKey()
		if err != nil {
			return nil, err
		}

		if !slices.Contains(revokedKids, k.Kid) {
			s.keys[k.Kid] = pub
		}
	}

	return s, nil
}

// Key returns the key of kid. A nil set has no key.
func (s *KeySet) Key(kid string) (ed25519.PublicKey, bool) {
	if s == nil {
		return nil, false
	}

	k, ok := s.keys[kid]

	return k, ok
}

// Len returns the number of usable keys.
func (s *KeySet) Len() int {
	if s == nil {
		return 0
	}

	return len(s.keys)
}
