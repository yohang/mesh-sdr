//go:build nogateway

package gateway

import "context"

// Gateway is absent from nogateway builds.
type Gateway struct{}

// Available reports whether this build embeds the gateway.
func Available() bool { return false }

// New returns ErrUnavailable.
func New(Options) (*Gateway, error) { return nil, ErrUnavailable }

// Start returns ErrUnavailable.
func (*Gateway) Start(context.Context) error { return ErrUnavailable }

// Stop does nothing.
func (*Gateway) Stop() error { return nil }
