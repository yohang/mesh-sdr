package wire

import (
	"context"
	"fmt"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	settingsapp "github.com/yohang/mesh-sdr/internal/settings/app"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// settingsAuditor writes the settings audit records to identity's audit_log
// (in the caller's transaction).
type settingsAuditor struct{ log identitydomain.AuditLog }

// Record implements settingsapp.Auditor.
func (a settingsAuditor) Record(ctx context.Context, r settingsapp.AuditRecord) error {
	actor := identitydomain.SystemActor()

	if !r.Actor.IsSystem() {
		id, err := identitydomain.NewUserID(r.Actor.User)
		if err != nil {
			return fmt.Errorf("audit actor: %w", err)
		}

		actor = identitydomain.UserActor(id, r.Actor.IP)
	}

	e, err := identitydomain.NewAuditEntry(r.At, actor, r.Action, identitydomain.AuditResult(r.Result))
	if err != nil {
		return fmt.Errorf("audit entry: %w", err)
	}

	e = e.WithTarget("setting", r.Key).WithRequestID(r.Actor.RequestID)

	if r.Before != "" {
		e = e.WithBefore(map[string]string{"value": r.Before})
	}

	if r.After != "" {
		e = e.WithAfter(map[string]string{"value": r.After})
	}

	return a.log.Append(ctx, e)
}

// settingsActor returns the signed-in user of a request as a settings actor.
func settingsActor(ctx context.Context) settingsapp.Actor {
	a := settingsapp.Actor{IP: clientip.From(ctx), RequestID: middleware.GetReqID(ctx)}

	if p := identityhttp.FromContext(ctx).Principal(); !p.IsAnonymous() {
		if u, err := shared.UUIDFromBytes(p.UserID().Bytes()); err == nil {
			a.User = u
		}
	}

	return a
}
