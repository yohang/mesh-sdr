//go:build unix

package keyring

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockDir takes an exclusive advisory lock on the keyring directory, held
// until the returned function runs: the hub and `meshsdr hub keys` never
// interleave a load and a save (a revocation cannot be lost).
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, fileMode) //nolint:gosec // fixed name in the keyring directory
	if err != nil {
		return nil, fmt.Errorf("keyring: lock: %w", err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { //nolint:gosec // a file descriptor fits in an int
		_ = f.Close()

		return nil, fmt.Errorf("keyring: lock: %w", err)
	}

	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:gosec // a file descriptor fits in an int
		_ = f.Close()
	}, nil
}
