package wire

import (
	"context"
)

// storeListenPolicy reads the global listen policy (listen_policy) from the
// settings store for the grid feature summary.
type storeListenPolicy struct {
	store interface{ String(key string) string }
}

// ListenPolicy implements grid/app.GlobalListenPolicy.
func (p storeListenPolicy) ListenPolicy(context.Context) (string, error) {
	return p.store.String("listen_policy"), nil
}
