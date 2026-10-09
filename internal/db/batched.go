package db

import "context"

// Batched calls fn with batch until it affects fewer rows, and returns the
// total: deletes in bounded batches keep each write transaction short
// (TECHNICAL_SPEC §7.3: at most 10 000 rows per transaction). It stops at
// the first error or when ctx is done.
func Batched(ctx context.Context, batch int, fn func(ctx context.Context, batch int) (int, error)) (int64, error) {
	var total int64

	for {
		n, err := fn(ctx, batch)
		total += int64(n)

		if err != nil || n < batch {
			return total, err
		}

		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
