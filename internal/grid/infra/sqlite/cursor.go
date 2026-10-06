package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// CursorRepository implements domain.EventCursorRepository.
type CursorRepository struct{ db db.Adapter }

// NewCursorRepository returns the repository.
func NewCursorRepository(a db.Adapter) *CursorRepository { return &CursorRepository{db: a} }

var _ domain.EventCursorRepository = (*CursorRepository)(nil)

// Last implements domain.EventCursorRepository.
func (r *CursorRepository) Last(ctx context.Context, id domain.NodeID, boot shared.UUID) (int64, error) {
	seq, err := sqlc.New(r.db.Reader(ctx)).GetEventCursor(ctx, sqlc.GetEventCursorParams{NodeID: id.String(), BootID: boot.Bytes()})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("event cursor of %s: %w", id, err)
	}

	return seq, nil
}

// Advance implements domain.EventCursorRepository.
func (r *CursorRepository) Advance(ctx context.Context, id domain.NodeID, boot shared.UUID, seq int64, now time.Time) error {
	err := sqlc.New(r.db.Writer(ctx)).UpsertEventCursor(ctx, sqlc.UpsertEventCursorParams{
		NodeID: id.String(), BootID: boot.Bytes(), LastSeq: seq, UpdatedAt: toMS(now),
	})
	if err != nil {
		return fmt.Errorf("advance event cursor of %s: %w", id, err)
	}

	return nil
}

// Forget implements domain.EventCursorRepository.
func (r *CursorRepository) Forget(ctx context.Context, id domain.NodeID, keep shared.UUID) error {
	if err := sqlc.New(r.db.Writer(ctx)).DeleteOldEventCursors(ctx, sqlc.DeleteOldEventCursorsParams{NodeID: id.String(), BootID: keep.Bytes()}); err != nil {
		return fmt.Errorf("forget event cursors of %s: %w", id, err)
	}

	return nil
}
