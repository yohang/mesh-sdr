package decodes

import (
	"context"
	"time"
)

// Retention job of the decoded messages (TECHNICAL_SPEC §7.3 decodes.purge,
// ADR 0028: one age for every mode, plus a row cap).
const (
	JobPurge   = "decodes.purge"
	PurgeEvery = time.Hour
)

// PurgeOld is the retention job: it deletes the messages older than the
// retention, then the oldest beyond the row cap, and returns the rows
// deleted.
func (m *Module) PurgeOld(ctx context.Context) (int64, error) {
	return m.repo.Purge(ctx, m.d.Now().Add(-m.d.Retention()), m.d.MaxRows())
}
