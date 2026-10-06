package wire

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	jobsapp "github.com/yohang/mesh-sdr/internal/jobs/app"
	jobssqlite "github.com/yohang/mesh-sdr/internal/jobs/infra/sqlite"
)

// jobs builds the hub's jobs scheduler with the retention jobs, and the
// retention view (ADM-011, ADR 0010).
func jobs(adapter db.Adapter, idm *identity.Module, values jobsapp.RetentionValues, audit identitydomain.AuditLog,
	logger *slog.Logger,
) (*jobsapp.Scheduler, *jobsapp.Retention, error) {
	sched := jobsapp.NewScheduler(jobssqlite.NewRuns(adapter), adapter, time.Now, component(logger, "jobs.app.scheduler"))
	sched.Register(idm.Reaper, identityapp.SessionReapEvery)
	sched.Register(idm.AuditPurger, identityapp.AuditPurgeEvery)

	sessions, err := jobssqlite.NewTableStats(adapter, "sessions")
	if err != nil {
		return nil, nil, err
	}

	auditLog, err := jobssqlite.NewTableStats(adapter, "audit_log")
	if err != nil {
		return nil, nil, err
	}

	stores := []jobsapp.Store{
		{Name: "sessions", Label: "Ended sessions", SettingKey: "retention.sessions", Job: identityapp.JobSessionsReap, Stats: sessions},
		{Name: "audit_log", Label: "Audit log", SettingKey: "retention.audit_log", Job: identityapp.JobAuditPurge, Stats: auditLog},
	}

	return sched, jobsapp.NewRetention(stores, sched, values, purgeAuditor{log: audit}, time.Now), nil
}

// purgeAuditor writes "purge now" records to identity's audit_log.
type purgeAuditor struct{ log identitydomain.AuditLog }

// RecordPurge implements jobsapp.Auditor.
func (a purgeAuditor) RecordPurge(ctx context.Context, r jobsapp.PurgeRecord) error {
	actor := identitydomain.SystemActor()

	if !r.Actor.User.IsZero() {
		id, err := identitydomain.NewUserID(r.Actor.User)
		if err != nil {
			return fmt.Errorf("audit actor: %w", err)
		}

		actor = identitydomain.UserActor(id, r.Actor.IP)
	}

	result := identitydomain.ResultOK
	if r.Failed {
		result = identitydomain.ResultError
	}

	e, err := identitydomain.NewAuditEntry(r.At, actor, jobsapp.ActionPurge, result)
	if err != nil {
		return fmt.Errorf("audit entry: %w", err)
	}

	e = e.WithTarget("store", r.Store).WithRequestID(r.Actor.RequestID).
		WithAfter(map[string]string{"rows_deleted": strconv.FormatInt(r.Rows, 10)})

	return a.log.Append(ctx, e)
}
