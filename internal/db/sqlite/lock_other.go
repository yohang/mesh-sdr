//go:build !unix

package sqlite

import (
	"context"
	"errors"
)

type fileLock struct{}

// lockFile is not supported outside unix: meshsdr targets Linux.
func lockFile(context.Context, string) (*fileLock, error) {
	return nil, errors.New("sqlite: migration lock requires a unix system")
}

func (*fileLock) unlock() error { return nil }
