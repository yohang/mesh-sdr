// Package sqlite implements the reporting outbox on the SQLite adapter
// (ADR 0006). Claims use a lease column written in the single writer
// transaction (§7.2: no SKIP LOCKED).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/reporting/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Outbox implements domain.Repository.
type Outbox struct{ db db.Adapter }

// NewOutbox returns the repository.
func NewOutbox(a db.Adapter) *Outbox { return &Outbox{db: a} }

var _ domain.Repository = (*Outbox)(nil)

// Enqueue implements domain.Repository.
func (r *Outbox) Enqueue(ctx context.Context, e *domain.Entry) (bool, error) {
	s := e.Snapshot()

	var decoded sql.NullInt64
	if s.DecodedID != nil {
		decoded = sql.NullInt64{Int64: *s.DecodedID, Valid: true}
	}

	n, err := sqlc.New(r.db.Writer(ctx)).EnqueueOutbox(ctx, sqlc.EnqueueOutboxParams{
		Network: string(s.Network), DecodedMessageID: decoded, Payload: string(s.Payload), DedupKey: s.Dedup[:],
		NextAttemptAt: s.NextAttempt.UnixMilli(), CreatedAt: s.CreatedAt.UnixMilli(),
	})
	if err != nil {
		return false, fmt.Errorf("enqueue %s entry: %w", s.Network, err)
	}

	return n > 0, nil
}

// Claim implements domain.Repository.
func (r *Outbox) Claim(ctx context.Context, network domain.Network, owner string, now, until time.Time, limit int) ([]*domain.Entry, error) {
	rows, err := sqlc.New(r.db.Writer(ctx)).ClaimOutbox(ctx, sqlc.ClaimOutboxParams{
		Owner: sql.NullString{String: owner, Valid: true}, LeaseUntil: sql.NullInt64{Int64: until.UnixMilli(), Valid: true},
		Network: string(network), Now: now.UnixMilli(), MaxRows: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("claim %s entries: %w", network, err)
	}

	out := make([]*domain.Entry, 0, len(rows))

	for _, row := range rows {
		e, err := fromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, e)
	}

	return out, nil
}

// Save implements domain.Repository.
func (r *Outbox) Save(ctx context.Context, e *domain.Entry, owner string) (bool, error) {
	s := e.Snapshot()

	n, err := sqlc.New(r.db.Writer(ctx)).SaveOutbox(ctx, sqlc.SaveOutboxParams{
		Status: string(s.Status), Attempts: int64(s.Attempts), NextAttemptAt: s.NextAttempt.UnixMilli(),
		LeaseOwner: nullString(s.LeaseOwner), LeaseUntil: nullMS(s.LeaseUntil), SentAt: nullMS(s.SentAt),
		LastError: nullString(s.LastError), ID: s.ID, Owner: sql.NullString{String: owner, Valid: true},
	})
	if err != nil {
		return false, fmt.Errorf("save outbox entry %d: %w", s.ID, err)
	}

	return n > 0, nil
}

// KillWaiting implements domain.Repository.
func (r *Outbox) KillWaiting(ctx context.Context, network domain.Network, before time.Time, reason string) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).KillWaitingOutbox(ctx, sqlc.KillWaitingOutboxParams{
		Reason: nullString(reason), Network: string(network), Cutoff: before.UnixMilli(),
	})
	if err != nil {
		return 0, fmt.Errorf("expire %s entries: %w", network, err)
	}

	return n, nil
}

// KillOverflow implements domain.Repository: the pending entries are
// counted first (index network, status, id), so an enqueue under the cap
// writes nothing more.
func (r *Outbox) KillOverflow(ctx context.Context, network domain.Network, keep int) (int64, error) {
	q := sqlc.New(r.db.Writer(ctx))

	pending, err := q.CountPendingOutbox(ctx, string(network))
	if err != nil {
		return 0, fmt.Errorf("count %s entries: %w", network, err)
	}

	if pending <= int64(keep) {
		return 0, nil
	}

	n, err := q.KillOverflowOutbox(ctx, sqlc.KillOverflowOutboxParams{
		Reason: nullString(domain.ReasonOverflow), Network: string(network), Keep: int64(keep),
	})
	if err != nil {
		return 0, fmt.Errorf("cap %s entries: %w", network, err)
	}

	return n, nil
}

// Purge implements domain.Repository.
func (r *Outbox) Purge(ctx context.Context, status domain.Status, before time.Time, limit int) (int64, error) {
	q := sqlc.New(r.db.Writer(ctx))

	var (
		n   int64
		err error
	)

	switch status {
	case domain.StatusSent:
		n, err = q.PurgeSentOutbox(ctx, sqlc.PurgeSentOutboxParams{
			Cutoff: sql.NullInt64{Int64: before.UnixMilli(), Valid: true}, MaxRows: int64(limit),
		})
	case domain.StatusDead:
		n, err = q.PurgeDeadOutbox(ctx, sqlc.PurgeDeadOutboxParams{Cutoff: before.UnixMilli(), MaxRows: int64(limit)})
	default:
		return 0, domain.ErrInvalidEntry.WithDetail("only sent and dead entries are purged")
	}

	if err != nil {
		return 0, fmt.Errorf("purge %s outbox entries: %w", status, err)
	}

	return n, nil
}

// Stats implements domain.Repository.
func (r *Outbox) Stats(ctx context.Context) ([]domain.NetworkStats, error) {
	q := sqlc.New(r.db.Reader(ctx))

	counts, err := q.OutboxCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("count outbox entries: %w", err)
	}

	times, err := q.OutboxTimes(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox times: %w", err)
	}

	byNetwork := map[domain.Network]*domain.NetworkStats{}

	for _, n := range domain.Networks {
		byNetwork[n] = &domain.NetworkStats{Network: n, Counts: map[domain.Status]int64{}}
	}

	for _, c := range counts {
		if s, ok := byNetwork[domain.Network(c.Network)]; ok {
			s.Counts[domain.Status(c.Status)] = c.Total
		}
	}

	for _, t := range times {
		s, ok := byNetwork[domain.Network(t.Network)]
		if !ok {
			continue
		}

		if t.LastSentAt > 0 {
			s.LastSentAt = time.UnixMilli(t.LastSentAt).UTC()
		}

		if t.OldestDueAt > 0 {
			s.OldestDueAt = time.UnixMilli(t.OldestDueAt).UTC()
		}
	}

	out := make([]domain.NetworkStats, 0, len(domain.Networks))
	for _, n := range domain.Networks {
		out = append(out, *byNetwork[n])
	}

	return out, nil
}

func fromRow(row sqlc.ReportingOutbox) (*domain.Entry, error) {
	var decoded *int64
	if row.DecodedMessageID.Valid {
		decoded = new(row.DecodedMessageID.Int64)
	}

	var key domain.DedupKey
	if len(row.DedupKey) != len(key) {
		return nil, fmt.Errorf("outbox entry %d: invalid dedup key", row.ID)
	}

	copy(key[:], row.DedupKey)

	var batch shared.UUID

	if row.BatchID != nil {
		b, err := shared.UUIDFromBytes(row.BatchID)
		if err != nil {
			return nil, fmt.Errorf("outbox entry %d batch: %w", row.ID, err)
		}

		batch = b
	}

	e, err := domain.Rehydrate(domain.Snapshot{
		ID: row.ID, Network: domain.Network(row.Network), DecodedID: decoded, Payload: json.RawMessage(row.Payload), Dedup: key,
		Status: domain.Status(row.Status), Attempts: int(row.Attempts), NextAttempt: time.UnixMilli(row.NextAttemptAt).UTC(),
		LeaseOwner: row.LeaseOwner.String, LeaseUntil: fromNullMS(row.LeaseUntil), Batch: batch,
		CreatedAt: time.UnixMilli(row.CreatedAt).UTC(), SentAt: fromNullMS(row.SentAt), LastError: row.LastError.String,
	})
	if err != nil {
		return nil, fmt.Errorf("stored outbox entry: %w", err)
	}

	return e, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}

	return time.UnixMilli(v.Int64).UTC()
}
