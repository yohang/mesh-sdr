package mapfeatures

import (
	"context"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
)

// The expiry job (TECHNICAL_SPEC §7.3 map.expire, MAP-012, ADR 0029): every
// 30 s it hard-deletes the features past their expires_at and publishes
// their removal.
const (
	JobExpire   = "map.expire"
	ExpireEvery = 30 * time.Second
)

// expireBatch bounds each delete of the expiry job.
const expireBatch = 1000

// Expire is the expiry job: it deletes the expired features in batches,
// publishes each batch once deleted and returns the rows deleted.
func (m *Module) Expire(ctx context.Context) (int64, error) {
	now := m.d.Now()

	return db.Batched(ctx, expireBatch, func(ctx context.Context, batch int) (int, error) {
		removed, err := m.repo.DeleteExpired(ctx, now, batch)
		if err != nil {
			return 0, err
		}

		if m.d.Published != nil {
			for i := range removed {
				m.d.Published(ctx, Change{Remove: &removed[i]})
			}
		}

		return len(removed), nil
	})
}
