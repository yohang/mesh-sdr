package wire

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	jobsapp "github.com/yohang/mesh-sdr/internal/jobs/app"
	jobsdomain "github.com/yohang/mesh-sdr/internal/jobs/domain"
	jobssqlite "github.com/yohang/mesh-sdr/internal/jobs/infra/sqlite"
	settingsapp "github.com/yohang/mesh-sdr/internal/settings/app"
	settingshttp "github.com/yohang/mesh-sdr/internal/settings/http"
)

// jobs builds the hub's jobs scheduler with the retention jobs, and the
// retention view (ADM-011, ADR 0010).
func jobs(adapter *db.DB, idm *identity.Module, sch *scheduling, values jobsapp.RetentionValues, audit identitydomain.AuditLog,
	logger *slog.Logger,
) (*jobsapp.Scheduler, *jobsapp.Retention, error) {
	sched := jobsapp.NewScheduler(jobssqlite.NewRuns(adapter), adapter, time.Now, component(logger, "jobs.app.scheduler"))
	sched.Register(idm.Reaper, identityapp.SessionReapEvery)
	sched.Register(idm.AuditPurger, identityapp.AuditPurgeEvery)
	// ADR 0020: the schedules' safety net and hourly push.
	sched.Register(sch.publish, SchedulesPublishEvery)

	connectionsPurge := gridapp.NewConnectionsPurge(gridsqlite.NewConnectionRepository(adapter),
		func() time.Duration { return values.Duration("retention.connections") }, time.Now)
	sched.Register(connectionsPurge, gridapp.ConnectionsPurgeEvery)

	for _, j := range idm.Purges {
		sched.Register(j, identityapp.LinkPurgeEvery)
	}

	sessions, err := jobssqlite.NewTableStats(adapter, "sessions")
	if err != nil {
		return nil, nil, err
	}

	auditLog, err := jobssqlite.NewTableStats(adapter, "audit_log")
	if err != nil {
		return nil, nil, err
	}

	connections, err := jobssqlite.NewTableStats(adapter, "connections")
	if err != nil {
		return nil, nil, err
	}

	stores := []jobsapp.Store{
		{Name: "sessions", Label: "Ended sessions", SettingKey: "retention.sessions", Job: identityapp.JobSessionsReap, Stats: sessions},
		{Name: "audit_log", Label: "Audit log", SettingKey: "retention.audit_log", Job: identityapp.JobAuditPurge, Stats: auditLog},
		{Name: "connections", Label: "Connections", SettingKey: "retention.connections", Job: gridapp.JobConnectionsPurge, Stats: connections},
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

// retentionRows adapts the retention view to the admin pages.
type retentionRows struct{ r *jobsapp.Retention }

// Stores implements settingshttp.Retention.
func (a retentionRows) Stores(ctx context.Context) ([]settingshttp.RetentionRow, error) {
	views, err := a.r.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]settingshttp.RetentionRow, 0, len(views))

	for _, v := range views {
		out = append(out, settingshttp.RetentionRow{
			Store: v.Store.Name, Label: v.Store.Label, SettingKey: v.Store.SettingKey, Retention: v.Retention,
			Rows: v.Rows, Bytes: v.Bytes, Sized: v.Sized, Running: v.LastRun.Running(), LastFinished: v.LastRun.LastFinished(),
			LastFailed: v.LastRun.Status() == jobsdomain.StatusError, LastError: v.LastRun.LastError(), LastRows: v.LastRun.Rows(),
		})
	}

	return out, nil
}

// Purge implements settingshttp.Retention.
func (a retentionRows) Purge(ctx context.Context, actor settingsapp.Actor, store string) (int64, error) {
	return a.r.Purge(ctx, jobsapp.Actor{User: actor.User, IP: actor.IP, RequestID: actor.RequestID}, store)
}
