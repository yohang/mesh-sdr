package wire

import (
	"context"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/files"
	"github.com/yohang/mesh-sdr/internal/jobs"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/settings"
)

// newJobs builds the hub's jobs scheduler with the retention jobs, and the
// retention view (ADM-011, ADR 0010).
func newJobs(adapter *db.DB, idm *identity.Module, sch *scheduling, values jobs.RetentionValues, audit audit.Appender,
	filesRetention *files.Retention, filesPolicy func() files.RetentionPolicy, logger *slog.Logger,
) (*jobs.Scheduler, *jobs.Retention, error) {
	sched := jobs.NewScheduler(jobs.NewRuns(adapter), adapter, time.Now, component(logger, "jobs.app.scheduler"))
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

	// FIL-004: the files the nodes sent, and the files left incomplete.
	sched.Register(filesRetention, files.RetentionEvery)

	sessions, err := jobs.NewTableStats(adapter, "sessions")
	if err != nil {
		return nil, nil, err
	}

	auditLog, err := jobs.NewTableStats(adapter, "audit_log")
	if err != nil {
		return nil, nil, err
	}

	connections, err := jobs.NewTableStats(adapter, "connections")
	if err != nil {
		return nil, nil, err
	}

	stores := []jobs.Store{
		{Name: "sessions", Label: "Ended sessions", SettingKey: "retention.sessions", Job: identityapp.JobSessionsReap, Stats: sessions},
		{Name: "audit_log", Label: "Audit log", SettingKey: "retention.audit_log", Job: identityapp.JobAuditPurge, Stats: auditLog},
		{Name: "connections", Label: "Connections", SettingKey: "retention.connections", Job: gridapp.JobConnectionsPurge, Stats: connections},
		{
			Name: "files", Label: "Files", Job: files.JobRetention, Stats: filesRetention,
			Policy: func() string { return filesPolicy().String() },
		},
	}

	return sched, jobs.NewRetention(stores, sched, values, audit), nil
}

// retentionRows adapts the retention view to the admin pages.
type retentionRows struct{ r *jobs.Retention }

// Stores implements settings.Retention.
func (a retentionRows) Stores(ctx context.Context) ([]settings.RetentionRow, error) {
	views, err := a.r.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]settings.RetentionRow, 0, len(views))

	for _, v := range views {
		out = append(out, settings.RetentionRow{
			Store: v.Store.Name, Label: v.Store.Label, SettingKey: v.Store.SettingKey, Retention: v.Retention, Policy: v.Policy,
			Rows: v.Rows, Bytes: v.Bytes, Sized: v.Sized, Running: v.LastRun.Running(), LastFinished: v.LastRun.LastFinished(),
			LastFailed: v.LastRun.Status() == jobs.StatusError, LastError: v.LastRun.LastError(), LastRows: v.LastRun.Rows(),
		})
	}

	return out, nil
}

// Purge implements settings.Retention.
func (a retentionRows) Purge(ctx context.Context, store string) (int64, error) {
	return a.r.Purge(ctx, store)
}
