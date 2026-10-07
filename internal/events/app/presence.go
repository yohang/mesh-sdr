package app

import (
	"context"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Connection is the presence record of one events socket (TECHNICAL_SPEC
// §7.3 "Heartbeat-based connection registry", rule 1).
type Connection struct {
	ID        shared.UUID
	UserID    shared.UUID // zero for an anonymous visitor
	SessionID shared.UUID // zero for an anonymous visitor
	RoleRank  int         // effective role at open time
	IP        string
	UserAgent string
}

// CloseReason tells why an events socket closed (§7.1 close_reason).
type CloseReason string

// Close reasons of events sockets.
const (
	// CloseClient: the client went away, or broke the protocol.
	CloseClient CloseReason = "client"
	// ClosePolicy: the session ended or was revoked.
	ClosePolicy CloseReason = "policy"
	// CloseHubRestart: the hub is shutting down.
	CloseHubRestart CloseReason = "hub_restart"
)

// Presence is the connection registry (the grid presence service): the
// events module opens a row per socket before its first application frame,
// refreshes the live rows in one batch and closes them.
type Presence interface {
	Open(ctx context.Context, c Connection) error
	Heartbeat(ctx context.Context, ids []shared.UUID) error
	// Attach records the device a viewer watches (presence.heartbeat).
	Attach(ctx context.Context, id shared.UUID, deviceID string) error
	Close(ctx context.Context, id shared.UUID, reason CloseReason) error
}
