package wire

import (
	"context"
	"log/slog"
	"strings"
	"time"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

// auditAppender is the audit_log repository of the identity module.
type auditAppender interface {
	Append(ctx context.Context, e identitydomain.AuditEntry) error
}

// gridAuditor writes the grid audit records to the identity audit_log (the
// Auditor port of ADR 0008 Q3). A record that cannot be written is logged.
type gridAuditor struct {
	log    auditAppender
	now    func() time.Time
	logger *slog.Logger
}

func (a gridAuditor) actor(ctx context.Context, kind string) identitydomain.Actor {
	switch kind {
	case gridapp.ActorUser:
		// REST calls: the client address resolved by the HTTP front
		// (trusted proxies applied), invalid outside a request.
		ip := clientip.From(ctx)

		if p := identityhttp.FromContext(ctx).Principal(); !p.IsAnonymous() {
			return identitydomain.UserActor(p.UserID(), ip)
		}

		return identitydomain.AnonymousActor(ip)
	case gridapp.ActorCLI:
		return identitydomain.CLIActor()
	default: // system, and node events (the node id is in the target or details)
		return identitydomain.SystemActor()
	}
}

// Record implements gridapp.Auditor.
func (a gridAuditor) Record(ctx context.Context, r gridapp.AuditRecord) {
	e, err := identitydomain.NewAuditEntry(a.now(), a.actor(ctx, r.ActorKind), r.Action, identitydomain.AuditResult(r.Result))
	if err == nil {
		target := "node"
		if strings.HasPrefix(r.Action, "device.") {
			target = "device"
		}

		e = e.WithTarget(target, r.Target)
		if len(r.Detail) > 0 {
			e = e.WithAfter(r.Detail)
		}

		err = a.log.Append(ctx, e)
	}

	if err != nil {
		a.logger.ErrorContext(ctx, "write grid audit record", slog.String("action", r.Action), slog.String("target", r.Target),
			slog.String("result", r.Result), slog.Any("error", err))
	}
}
