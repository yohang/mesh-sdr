package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// AuditLog is the append-only SQLite audit repository. The table also has a
// trigger that aborts any UPDATE.
type AuditLog struct{ db db.Adapter }

var _ domain.AuditLog = (*AuditLog)(nil)

// NewAuditLog returns the repository.
func NewAuditLog(a db.Adapter) *AuditLog { return &AuditLog{db: a} }

// Append inserts an entry.
func (r *AuditLog) Append(ctx context.Context, e domain.AuditEntry) error {
	before, err := jsonMap(e.Before())
	if err != nil {
		return err
	}

	after, err := jsonMap(e.After())
	if err != nil {
		return err
	}

	var actorID []byte
	if id := e.Actor().UserID(); !id.IsZero() {
		actorID = id.Bytes()
	}

	err = sqlc.New(r.db.Writer(ctx)).InsertAuditEntry(ctx, sqlc.InsertAuditEntryParams{
		At:          ms(e.At()),
		ActorKind:   string(e.Actor().Kind()),
		ActorUserID: actorID,
		ActorIp:     nullIP(e.Actor().IP()),
		Action:      e.Action(),
		TargetType:  nullString(e.TargetType()),
		TargetID:    nullString(e.TargetID()),
		Result:      string(e.Result()),
		Before:      before,
		After:       after,
		RequestID:   nullString(e.RequestID()),
	})
	if err != nil {
		return fmt.Errorf("append audit entry %s: %w", e.Action(), err)
	}

	return nil
}

func jsonMap(m map[string]string) (sql.NullString, error) {
	if m == nil {
		return sql.NullString{}, nil
	}

	b, err := json.Marshal(m)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode audit detail: %w", err)
	}

	return sql.NullString{String: string(b), Valid: true}, nil
}

// Recent returns the latest entries, newest first.
func (r *AuditLog) Recent(ctx context.Context, limit int) ([]domain.AuditEntry, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListRecentAuditEntries(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}

	out := make([]domain.AuditEntry, 0, len(rows))

	for _, row := range rows {
		var actor domain.Actor

		ip := parseIP(row.ActorIp)

		switch domain.ActorKind(row.ActorKind) {
		case domain.ActorUser:
			id, err := domain.UserIDFromBytes(row.ActorUserID)
			if err != nil {
				return nil, fmt.Errorf("audit entry %d: %w", row.ID, err)
			}

			actor = domain.UserActor(id, ip)
		case domain.ActorAnonymous:
			actor = domain.AnonymousActor(ip)
		case domain.ActorCLI:
			actor = domain.CLIActor()
		default:
			actor = domain.SystemActor()
		}

		e, err := domain.NewAuditEntry(fromMS(row.At), actor, row.Action, domain.AuditResult(row.Result))
		if err != nil {
			return nil, fmt.Errorf("audit entry %d: %w", row.ID, err)
		}

		e = e.WithTarget(row.TargetType.String, row.TargetID.String).WithRequestID(row.RequestID.String)

		if row.Before.Valid {
			var m map[string]string
			if err := json.Unmarshal([]byte(row.Before.String), &m); err == nil {
				e = e.WithBefore(m)
			}
		}

		if row.After.Valid {
			var m map[string]string
			if err := json.Unmarshal([]byte(row.After.String), &m); err == nil {
				e = e.WithAfter(m)
			}
		}

		out = append(out, e)
	}

	return out, nil
}

// DeleteBefore implements domain.AuditPurge.
func (r *AuditLog) DeleteBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteAuditEntriesBefore(ctx, sqlc.DeleteAuditEntriesBeforeParams{Cutoff: ms(cutoff), Batch: int64(limit)})
	if err != nil {
		return 0, fmt.Errorf("delete audit entries: %w", err)
	}

	return int(n), nil
}
