// Package tokenkey is the interim access-token key of the hub: one Ed25519
// key generated per hub process, held in RAM only. It implements the grid
// app.KeySource and app.TokenIssuer ports until the identity keyring
// (ACC-007: rotation, persistence, revocation) replaces it.
//
// A hub restart changes the key: the nodes receive the new one when their
// control channel reconnects, and media connections opened before the
// restart are lost anyway (the gateway restarts with the hub).
package tokenkey

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// Ephemeral is one in-memory signing key.
type Ephemeral struct {
	key ed25519.PrivateKey
	jwk token.JWK
}

var (
	_ app.KeySource   = (*Ephemeral)(nil)
	_ app.TokenIssuer = (*Ephemeral)(nil)
)

// NewEphemeral generates the key.
func NewEphemeral() (*Ephemeral, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate access-token key: %w", err)
	}

	return &Ephemeral{key: key, jwk: token.NewJWK(pub)}, nil
}

// Issue implements app.TokenIssuer.
func (e *Ephemeral) Issue(_ context.Context, c token.Claims) (string, error) {
	return token.Sign(c, e.key)
}

// VerificationKeys implements app.KeySource.
func (e *Ephemeral) VerificationKeys(context.Context) (app.VerificationKeys, error) {
	return app.VerificationKeys{Keys: []token.JWK{e.jwk}, RevokedKids: []string{}}, nil
}

// Changed implements app.KeySource: the key never changes.
func (e *Ephemeral) Changed() <-chan struct{} { return nil }
