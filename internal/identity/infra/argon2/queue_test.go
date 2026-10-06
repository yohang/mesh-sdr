package argon2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestFullQueueIsRateLimited(t *testing.T) {
	ctx := context.Background()
	h := New(Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}, 1, 1)

	hash, err := h.Hash(ctx, "password")
	if err != nil {
		t.Fatal(err)
	}

	h.sem <- struct{}{} // the only slot is busy

	// One request may wait…
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := h.Verify(waitCtx, "password", hash)
		done <- err
	}()

	for h.waiting.Load() != 1 {
		time.Sleep(time.Millisecond)
	}

	// …the next one is refused at once.
	_, err = h.Verify(ctx, "password", hash)

	var rl *domain.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter() != time.Second {
		t.Errorf("Verify with a full queue = %v", err)
	}

	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued Verify = %v", err)
	}

	<-h.sem

	if ok, err := h.Verify(ctx, "password", hash); !ok || err != nil {
		t.Errorf("Verify after the slot is free = %v, %v", ok, err)
	}
}
