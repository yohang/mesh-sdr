package http

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// Headers of the forward-auth answer (§4.6, §5.16).
const (
	HeaderAccessToken = "X-Rx-Access-Token"
	HeaderCID         = "X-Rx-Cid"
	// HeaderUpstream names the node address for the gateway; it never
	// leaves the hub.
	HeaderUpstream = "X-Rx-Upstream"
)

// AuthzHandler is the hub forward auth of /nodes/{nodeId}/ws, called
// in-process by the gateway at gateway.AuthzPath?node=<id>. It answers 204
// with the access token, or a problem+json passed to the browser as is.
type AuthzHandler struct {
	access   *app.MediaAccess
	subject  func(r *http.Request) app.Subject
	clientIP func(r *http.Request) string
	logger   *slog.Logger
}

// NewAuthzHandler returns the handler. subject resolves the session of the
// request, clientIP its client address.
func NewAuthzHandler(access *app.MediaAccess, subject func(r *http.Request) app.Subject,
	clientIP func(r *http.Request) string, logger *slog.Logger,
) *AuthzHandler {
	return &AuthzHandler{access: access, subject: subject, clientIP: clientIP, logger: logger}
}

func (h *AuthzHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")

	grant, err := h.access.Authorize(r.Context(), app.AuthzRequest{
		NodeID: node, Subject: h.subject(r), Origin: r.Header.Get("Origin"), IP: h.clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		p := problem.FromError(err)

		if wait, ok := app.IsRateLimited(err); ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		}

		level := slog.LevelDebug
		if p.Status >= http.StatusInternalServerError && p.Status != http.StatusServiceUnavailable {
			level = slog.LevelError
		}

		h.logger.LogAttrs(r.Context(), level, "media connection refused",
			slog.String("node_id", node), slog.Int("status", p.Status), slog.String("code", p.Code), slog.Any("error", err))
		problem.Write(w, p)

		return
	}

	w.Header().Set(HeaderAccessToken, grant.Token)
	w.Header().Set(HeaderCID, grant.CID)
	w.Header().Set(HeaderUpstream, grant.Upstream)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
