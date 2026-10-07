package wire

import (
	"context"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
)

// auditAppender implements audit.Appender over identity's audit_log, for every
// module but identity (which writes its own entries).
type auditAppender struct {
	log interface {
		Append(ctx context.Context, e identitydomain.AuditEntry) error
	}
	now func() time.Time
}

// Append resolves the actor (audit.Caller: the signed-in user of the
// request in ctx, or an anonymous client, at the client address resolved by
// the HTTP front) and appends the entry.
func (a auditAppender) Append(ctx context.Context, r audit.Record) error {
	var actor identitydomain.Actor

	switch r.Actor {
	case audit.System:
		actor = identitydomain.SystemActor()
	case audit.CLI:
		actor = identitydomain.CLIActor()
	default:
		ip := clientip.From(ctx)
		actor = identitydomain.AnonymousActor(ip)

		if p := identityhttp.FromContext(ctx).Principal(); !p.IsAnonymous() {
			actor = identitydomain.UserActor(p.UserID(), ip)
		}
	}

	result := identitydomain.AuditResult(r.Result)
	if result == "" {
		result = identitydomain.ResultOK
	}

	e, err := identitydomain.NewAuditEntry(a.now(), actor, r.Action, result)
	if err != nil {
		return fmt.Errorf("audit entry %s: %w", r.Action, err)
	}

	e = e.WithTarget(r.TargetType, r.TargetID).WithRequestID(middleware.GetReqID(ctx))

	if len(r.Before) > 0 {
		e = e.WithBefore(r.Before)
	}

	if len(r.After) > 0 {
		e = e.WithAfter(r.After)
	}

	return a.log.Append(ctx, e)
}

// newAuditAppender returns the audit appender over the audit_log of adapter.
func newAuditAppender(adapter *db.DB, now func() time.Time) auditAppender {
	return auditAppender{log: identitysqlite.NewAuditLog(adapter), now: now}
}

// currentUser returns the signed-in user of a request (zero when anonymous).
func currentUser(ctx context.Context) shared.UUID {
	if p := identityhttp.FromContext(ctx).Principal(); !p.IsAnonymous() {
		if u, err := shared.UUIDFromBytes(p.UserID().Bytes()); err == nil {
			return u
		}
	}

	return shared.UUID{}
}
