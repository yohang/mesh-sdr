//go:build unix

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockPollInterval is how often a blocked lock attempt is retried.
const lockPollInterval = 50 * time.Millisecond

// fileLock is an exclusive advisory lock (flock) held on a file.
type fileLock struct {
	f *os.File
}

// lockFile takes an exclusive flock on path (created 0600 if absent),
// waiting until it is free or ctx is done. goose has no SQLite locker, so
// this keeps two `meshsdr hub migrate` runs from interleaving.
func lockFile(ctx context.Context, path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // derived from the operator's db.dsn
	if err != nil {
		return nil, fmt.Errorf("sqlite: open lock %s: %w", path, err)
	}

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fd fits in int
		if err == nil {
			return &fileLock{f: f}, nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()

			return nil, fmt.Errorf("sqlite: lock %s: %w", path, err)
		}

		select {
		case <-ctx.Done():
			_ = f.Close()

			return nil, fmt.Errorf("sqlite: wait for lock %s (another migration is running?): %w", path, ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// unlock releases the lock. Closing the file releases the flock.
func (l *fileLock) unlock() error {
	return l.f.Close()
}
