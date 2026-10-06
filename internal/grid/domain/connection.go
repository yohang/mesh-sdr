package domain

import (
	"context"
	"net/netip"
	"slices"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ConnectionKind is the kind of a client connection (§7.1 connections).
type ConnectionKind string

// Connection kinds.
const (
	ConnectionEvents ConnectionKind = "events"
	ConnectionMedia  ConnectionKind = "media"
	ConnectionMap    ConnectionKind = "map"
)

// CloseReason tells why a connection closed (§7.1, §4.9).
type CloseReason string

// Close reasons.
const (
	CloseClient           CloseReason = "client"
	CloseHeartbeatTimeout CloseReason = "heartbeat_timeout"
	CloseHubRestart       CloseReason = "hub_restart"
	CloseNodeLost         CloseReason = "node_lost"
	CloseNodeRestart      CloseReason = "node_restart"
	ClosePolicy           CloseReason = "policy"
)

// ParseCloseReason validates a reported close reason; unknown values are
// recorded as "client".
func ParseCloseReason(s string) CloseReason {
	r := CloseReason(s)
	if slices.Contains([]CloseReason{CloseClient, CloseHeartbeatTimeout, CloseHubRestart, CloseNodeLost, CloseNodeRestart, ClosePolicy}, r) {
		return r
	}

	return CloseClient
}

// NormalizeIP returns the canonical text of an IP, IPv4-mapped IPv6
// addresses as IPv4 (§7.1 "IP addresses"); "" when s is not an IP.
func NormalizeIP(s string) string {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return ""
	}

	return a.Unmap().String()
}

// ConnectionInfo describes a new connection.
type ConnectionInfo struct {
	ID        shared.UUID
	Kind      ConnectionKind
	UserID    shared.UUID
	SessionID shared.UUID
	RoleID    int
	IP        string
	UserAgent string
	NodeID    string
	DeviceID  string
	Mode      string
}

// Connection is one presence registry row.
type Connection struct {
	info          ConnectionInfo
	presetID      shared.UUID
	tunedFreq     *int64
	secondaryMode string
	openedAt      time.Time
	lastHeartbeat time.Time
	closedAt      time.Time
	closeReason   CloseReason
	bytesOut      int64
	bytesIn       int64
}

// NewConnection opens a connection at now.
func NewConnection(info ConnectionInfo, now time.Time) (*Connection, error) {
	switch {
	case info.ID.IsZero():
		return nil, ErrInvalidConnection.WithDetail("connection id is required")
	case info.Kind != ConnectionEvents && info.Kind != ConnectionMedia && info.Kind != ConnectionMap:
		return nil, ErrInvalidConnection.WithDetail("invalid connection kind")
	case info.RoleID < 0 || info.RoleID > 32767:
		return nil, ErrInvalidConnection.WithDetail("invalid role")
	}

	info.IP = NormalizeIP(info.IP)
	info.UserAgent = cleanText(info.UserAgent, 512)
	info.Mode = cleanText(info.Mode, 24)

	if info.NodeID != "" {
		if _, err := NewNodeID(info.NodeID); err != nil {
			return nil, err
		}
	}

	if info.DeviceID != "" {
		if _, err := NewDeviceID(info.DeviceID); err != nil {
			return nil, err
		}
	}

	now = ms(now)

	return &Connection{info: info, openedAt: now, lastHeartbeat: now}, nil
}

// Info returns the connection identity.
func (c *Connection) Info() ConnectionInfo { return c.info }

// OpenedAt returns the open time.
func (c *Connection) OpenedAt() time.Time { return c.openedAt }

// LastHeartbeat returns the last liveness evidence.
func (c *Connection) LastHeartbeat() time.Time { return c.lastHeartbeat }

// Closed reports whether the connection is closed, when and why.
func (c *Connection) Closed() (time.Time, CloseReason, bool) {
	return c.closedAt, c.closeReason, !c.closedAt.IsZero()
}

// Heartbeat records liveness evidence.
func (c *Connection) Heartbeat(now time.Time) {
	if c.closedAt.IsZero() {
		c.lastHeartbeat = ms(now)
	}
}

// Attach records the current device and mode of a media connection.
func (c *Connection) Attach(deviceID, mode string, now time.Time) {
	if deviceID != "" {
		if _, err := NewDeviceID(deviceID); err == nil {
			c.info.DeviceID = deviceID
		}
	}

	if mode != "" {
		c.info.Mode = cleanText(mode, 24)
	}

	c.Heartbeat(now)
}

// Close closes the connection; closing twice keeps the first close.
func (c *Connection) Close(reason CloseReason, at time.Time) bool {
	if !c.closedAt.IsZero() {
		return false
	}

	c.closedAt = ms(at)
	c.closeReason = reason

	return true
}

// ConnectionSnapshot is the persisted form of a Connection.
type ConnectionSnapshot struct {
	Info          ConnectionInfo
	PresetID      shared.UUID
	TunedFreq     *int64
	SecondaryMode string
	OpenedAt      time.Time
	LastHeartbeat time.Time
	ClosedAt      time.Time
	CloseReason   CloseReason
	BytesOut      int64
	BytesIn       int64
}

// Snapshot returns the persisted form.
func (c *Connection) Snapshot() ConnectionSnapshot {
	return ConnectionSnapshot{
		Info: c.info, PresetID: c.presetID, TunedFreq: c.tunedFreq, SecondaryMode: c.secondaryMode,
		OpenedAt: c.openedAt, LastHeartbeat: c.lastHeartbeat, ClosedAt: c.closedAt, CloseReason: c.closeReason,
		BytesOut: c.bytesOut, BytesIn: c.bytesIn,
	}
}

// RehydrateConnection rebuilds a Connection.
func RehydrateConnection(s ConnectionSnapshot) (*Connection, error) {
	if s.Info.ID.IsZero() {
		return nil, ErrInvalidConnection
	}

	return &Connection{
		info: s.Info, presetID: s.PresetID, tunedFreq: s.TunedFreq, secondaryMode: s.SecondaryMode,
		openedAt: s.OpenedAt, lastHeartbeat: s.LastHeartbeat, closedAt: s.ClosedAt, closeReason: s.CloseReason,
		bytesOut: s.BytesOut, bytesIn: s.BytesIn,
	}, nil
}

// ConnectionRepository persists the presence registry.
type ConnectionRepository interface {
	// Open inserts c; an existing id is left untouched (false).
	Open(ctx context.Context, c *Connection) (bool, error)
	Get(ctx context.Context, id shared.UUID) (*Connection, error)
	Save(ctx context.Context, c *Connection) error
	// Heartbeat refreshes the open rows of ids in one statement.
	Heartbeat(ctx context.Context, ids []shared.UUID, now time.Time) (int64, error)
	// CloseStale closes open rows silent since before with heartbeat_timeout.
	CloseStale(ctx context.Context, before time.Time) (int64, error)
	CloseAll(ctx context.Context, reason CloseReason, at time.Time) (int64, error)
	CloseNode(ctx context.Context, node NodeID, reason CloseReason, at time.Time) (int64, error)
	ListOpen(ctx context.Context) ([]*Connection, error)
	CountOpen(ctx context.Context) (int, error)
	// CountOpenNode counts the open rows of a node.
	CountOpenNode(ctx context.Context, node NodeID) (int, error)
	// OpenNodes lists the nodes with open media rows.
	OpenNodes(ctx context.Context) ([]NodeID, error)
	// DeleteClosedBefore applies the retention.
	DeleteClosedBefore(ctx context.Context, before time.Time) (int64, error)
}
