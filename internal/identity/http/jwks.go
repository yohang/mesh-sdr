package http

import (
	"encoding/json"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// JWKSPath publishes the token verification keys (TECHNICAL_SPEC §6.10:
// informational; nodes get them over the control channel).
const JWKSPath = "/.well-known/jwks.json"

// jwksDocument is the JWKS with the revoked kids.
type jwksDocument struct {
	token.JWKS

	RevokedKids []string `json:"revoked_kids"`
}

func (m *Module) jwks(w http.ResponseWriter, r *http.Request) {
	if m.keys == nil {
		m.pages.Error(w, r, http.StatusNotFound)

		return
	}

	jwks, revoked := m.keys.Published(m.now())

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(jwksDocument{JWKS: jwks, RevokedKids: revoked})
	}
}
