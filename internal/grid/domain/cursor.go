package domain

import (
	"context"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// EventCursorRepository tracks the last node event persisted per node boot
// (§4.4 idempotent replay, §7.1 node_event_cursor).
type EventCursorRepository interface {
	// Last returns the last applied seq of (node, boot), 0 when none.
	Last(ctx context.Context, id NodeID, boot shared.UUID) (int64, error)
	// Advance records seq as the last applied event of (node, boot).
	Advance(ctx context.Context, id NodeID, boot shared.UUID, seq int64, now time.Time) error
	// Forget drops the cursors of every other boot of the node.
	Forget(ctx context.Context, id NodeID, keep shared.UUID) error
}
