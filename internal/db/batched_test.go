package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
)

func TestBatched(t *testing.T) {
	left := 25

	n, err := db.Batched(context.Background(), 10, func(_ context.Context, batch int) (int, error) {
		k := min(batch, left)
		left -= k

		return k, nil
	})
	if err != nil || n != 25 || left != 0 {
		t.Errorf("batched = %d, %v, left %d", n, err, left)
	}

	boom := errors.New("disk full")
	if n, err := db.Batched(context.Background(), 10, func(context.Context, int) (int, error) { return 10, boom }); n != 10 || !errors.Is(err, boom) {
		t.Errorf("failed batch = %d, %v", n, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if n, err := db.Batched(ctx, 10, func(context.Context, int) (int, error) { return 10, nil }); n != 10 || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled = %d, %v", n, err)
	}
}
