package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// TokenService issues access tokens.
type TokenService interface {
	Mint(ctx context.Context, in app.MintInput) (app.Minted, error)
}

// SessionRefs gives the public handle of the request's session.
type SessionRefs interface {
	SessionRef(ctx context.Context) string
}

// TokenHandlers serve /auth/token.
type TokenHandlers struct {
	sessions Sessions
	refs     SessionRefs
	tokens   TokenService
}

// NewTokenHandlers returns the handlers.
func NewTokenHandlers(s Sessions, r SessionRefs, t TokenService) TokenHandlers {
	return TokenHandlers{sessions: s, refs: r, tokens: t}
}

// MintAccessToken implements StrictServerInterface.
func (h TokenHandlers) MintAccessToken(ctx context.Context, req MintAccessTokenRequestObject) (MintAccessTokenResponseObject, error) {
	m, err := h.tokens.Mint(ctx, app.MintInput{
		By: h.sessions.Actor(ctx), SessionRef: h.refs.SessionRef(ctx), NodeID: req.Body.NodeId, ConnectionID: req.Body.Cid,
	})

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return jsonOK{AccessToken{Token: m.Raw, ExpiresAt: m.ExpiresAt.UTC()}}, nil
}

func (j jsonOK) VisitMintAccessTokenResponse(w http.ResponseWriter) error { return j.write(w) }

func (r rateLimited) VisitMintAccessTokenResponse(w http.ResponseWriter) error { return r.write(w) }
