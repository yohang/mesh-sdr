package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/yohang/mesh-sdr/internal/jobs/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrUnknownStore is a retention store that does not exist.
var ErrUnknownStore = shared.NewError(shared.KindNotFound, "unknown_store", "unknown retention store")

// Stats measures a store: row count, and size in bytes when sized.
type Stats interface {
	Stats(ctx context.Context) (rows, bytes int64, sized bool, err error)
}

// Store is a DB-backed store with a retention policy: the setting holding
// its retention, and the job that applies it.
type Store struct {
	Name       string // "sessions", "audit_log"
	Label      string
	SettingKey string // "retention.sessions"
	Job        string // "sessions.reap"
	Stats      Stats
}

// RetentionValues reads the current retention settings.
type RetentionValues interface {
	Duration(key string) time.Duration
}

// Actor is who asks for a purge.
type Actor struct {
	User      shared.UUID
	IP        netip.Addr
	RequestID string
}

// ActionPurge is the audited action of "purge now".
const ActionPurge = "retention.purge"

// PurgeRecord is an audited "purge now".
type PurgeRecord struct {
	Actor  Actor
	At     time.Time
	Store  string
	Rows   int64
	Failed bool
}

// Auditor appends audit records.
type Auditor interface {
	RecordPurge(ctx context.Context, r PurgeRecord) error
}

// Retention is the retention policies view (ADM-011): the stores that
// exist, their retention, size and last purge, and "purge now".
type Retention struct {
	stores    []Store
	scheduler *Scheduler
	values    RetentionValues
	audit     Auditor
	now       Clock
}

// NewRetention returns the use case over stores.
func NewRetention(stores []Store, scheduler *Scheduler, values RetentionValues, audit Auditor, now Clock) *Retention {
	return &Retention{stores: append([]Store(nil), stores...), scheduler: scheduler, values: values, audit: audit, now: now}
}

// StoreView is one store of the retention view.
type StoreView struct {
	Store     Store
	Retention time.Duration
	Rows      int64
	Bytes     int64
	Sized     bool
	LastRun   *domain.Run
}

// List returns every store in registration order.
func (r *Retention) List(ctx context.Context) ([]StoreView, error) {
	out := make([]StoreView, 0, len(r.stores))

	for _, s := range r.stores {
		rows, bytes, sized, err := s.Stats.Stats(ctx)
		if err != nil {
			return nil, fmt.Errorf("stats of %s: %w", s.Name, err)
		}

		run, err := r.scheduler.LastRun(ctx, s.Job)
		if err != nil {
			return nil, fmt.Errorf("last run of %s: %w", s.Job, err)
		}

		out = append(out, StoreView{
			Store: s, Retention: r.values.Duration(s.SettingKey), Rows: rows, Bytes: bytes, Sized: sized, LastRun: run,
		})
	}

	return out, nil
}

// Purge applies the retention policy of a store now ("purge now"): it runs
// its job (which never deletes rows younger than the retention) and audits
// it. It returns the rows deleted, ErrUnknownStore or domain.ErrJobRunning.
func (r *Retention) Purge(ctx context.Context, actor Actor, store string) (int64, error) {
	for _, s := range r.stores {
		if s.Name != store {
			continue
		}

		rows, err := r.scheduler.RunNow(ctx, s.Job)
		if errors.Is(err, domain.ErrJobRunning) {
			return 0, err
		}

		rec := PurgeRecord{Actor: actor, At: r.now(), Store: s.Name, Rows: rows, Failed: err != nil}
		if aerr := r.audit.RecordPurge(ctx, rec); aerr != nil {
			return rows, errors.Join(err, fmt.Errorf("audit purge of %s: %w", s.Name, aerr))
		}

		return rows, err
	}

	return 0, ErrUnknownStore.WithDetail("unknown retention store " + store)
}
