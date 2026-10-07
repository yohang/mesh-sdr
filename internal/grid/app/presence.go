package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// MaxOpenConnectionsPerNode bounds the open presence rows a node can
// create through its control channel (ADR 0008).
const MaxOpenConnectionsPerNode = 1000

// Presence is the heartbeat-based connection registry (§7.3, GRID-017).
type Presence struct {
	repo    domain.ConnectionRepository
	devices domain.DeviceRepository
	tracker *Tracker
	timings *timingsCell
	now     Clock
	logger  *slog.Logger
	changed []func(ctx context.Context)
}

// OnChange registers a callback run after open rows may have changed:
// opened, closed or reaped by this service (composition time only). Rows
// written by control-channel events are reported by Control.OnApplied.
func (s *Presence) OnChange(f func(ctx context.Context)) { s.changed = append(s.changed, f) }

func (s *Presence) notify(ctx context.Context) {
	for _, f := range s.changed {
		f(ctx)
	}
}

// NewPresence returns the service. tracker may be nil when the grid is
// disabled.
func NewPresence(repo domain.ConnectionRepository, devices domain.DeviceRepository, tracker *Tracker, timings Timings, now Clock, logger *slog.Logger) *Presence {
	return &Presence{repo: repo, devices: devices, tracker: tracker, timings: newTimingsCell(timings), now: now, logger: logger}
}

// SetTimings replaces the timings (settings change).
func (s *Presence) SetTimings(t Timings) { s.timings.set(t) }

// Open records a new connection (hub events WS, gateway authz).
func (s *Presence) Open(ctx context.Context, info domain.ConnectionInfo) error {
	c, err := domain.NewConnection(info, s.now())
	if err != nil {
		return err
	}

	if _, err := s.repo.Open(ctx, c); err != nil {
		return err
	}

	s.notify(ctx)

	return nil
}

// Attach records the device a connection watches.
func (s *Presence) Attach(ctx context.Context, id shared.UUID, deviceID string) error {
	c, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}

	c.Attach(deviceID, "", s.now())

	return s.repo.Save(ctx, c)
}

// Heartbeat refreshes the open connections ids in one statement.
func (s *Presence) Heartbeat(ctx context.Context, ids []shared.UUID) error {
	_, err := s.repo.Heartbeat(ctx, ids, s.now())

	return err
}

// Close closes one connection.
func (s *Presence) Close(ctx context.Context, id shared.UUID, reason domain.CloseReason) error {
	c, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}

	if !c.Close(reason, s.now()) {
		return nil
	}

	if err := s.repo.Save(ctx, c); err != nil {
		return err
	}

	s.notify(ctx)

	return nil
}

// CloseAtStart closes every row left open by the previous hub process
// (§7.3 rule 4).
func (s *Presence) CloseAtStart(ctx context.Context) error {
	n, err := s.repo.CloseAll(ctx, domain.CloseHubRestart, s.now())
	if err != nil {
		return err
	}

	if n > 0 {
		s.logger.InfoContext(ctx, "connections left open by the previous hub closed", slog.Int64("count", n))
		s.notify(ctx)
	}

	return nil
}

// Reap closes stale rows and the rows of nodes lost for longer than the
// stale delay. Closed rows are deleted by the connections.purge job.
func (s *Presence) Reap(ctx context.Context) {
	now := s.now()
	closed := int64(0)

	if n, err := s.repo.CloseStale(ctx, now.Add(-s.timings.get().PresenceStale)); err != nil {
		s.logger.ErrorContext(ctx, "close stale connections", slog.Any("error", err))
	} else if n > 0 {
		closed += n
		s.logger.DebugContext(ctx, "stale connections closed", slog.Int64("count", n))
	}

	if s.tracker != nil {
		nodes, err := s.repo.OpenNodes(ctx)
		if err != nil {
			s.logger.ErrorContext(ctx, "list nodes with connections", slog.Any("error", err))
		}

		for _, id := range nodes {
			st, ok := s.tracker.State(id)
			if ok && !st.Connected && !st.DisconnectedAt.IsZero() && now.Sub(st.DisconnectedAt) > s.timings.get().PresenceStale {
				n, err := s.repo.CloseNode(ctx, id, domain.CloseNodeLost, now)
				if err != nil {
					s.logger.ErrorContext(ctx, "close connections of a lost node", slog.String("node_id", id.String()), slog.Any("error", err))
				}

				closed += n
			}
		}
	}

	if closed > 0 {
		s.notify(ctx)
	}
}

// Run reaps every 15 s (or every stale/3 when shorter) until ctx is done.
func (s *Presence) Run(ctx context.Context) {
	every := s.reapEvery()
	t := time.NewTicker(every)

	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reap(ctx)
		case <-s.timings.changes:
		}

		// The period follows the timings (SetTimings).
		if e := s.reapEvery(); e != every {
			every = e
			t.Reset(every)
		}
	}
}

func (s *Presence) reapEvery() time.Duration {
	return max(min(15*time.Second, s.timings.get().PresenceStale/3), 10*time.Millisecond)
}

// NodeRestarted closes the rows of a node's previous boot (a BootHandler).
func (s *Presence) NodeRestarted(ctx context.Context, id domain.NodeID, now time.Time) error {
	_, err := s.repo.CloseNode(ctx, id, domain.CloseNodeRestart, now)

	return err
}

// Handler applies connection.opened, connection.heartbeat and
// connection.closed events of media sessions (control ingestion).
func (s *Presence) Handler() EventHandler {
	return func(ctx context.Context, n *domain.Node, ev Event, now time.Time) error {
		p, err := decodeEvent[ctl.Connection](ev)
		if err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}

		id, err := shared.ParseUUID(p.CID)
		if err != nil {
			s.logger.WarnContext(ctx, "connection event with an invalid cid skipped", slog.String("node_id", n.ID().String()))

			return nil
		}

		c, err := s.repo.Get(ctx, id)

		switch {
		case errors.Is(err, domain.ErrConnectionNotFound):
			if ev.Type == rxv1.TypeConnectionClosed {
				return nil
			}

			open, err := s.repo.CountOpenNode(ctx, n.ID())
			if err != nil {
				return err
			}

			if open >= MaxOpenConnectionsPerNode {
				s.logger.WarnContext(ctx, "connection event refused: too many open connections for the node",
					slog.String("node_id", n.ID().String()), slog.Int("open", open))

				return nil
			}

			device, err := s.ownedDevice(ctx, n.ID(), p.DeviceID)
			if err != nil {
				return err
			}

			user, _ := shared.ParseUUID(p.UserID)
			session, _ := shared.ParseUUID(p.SID)

			c, err = domain.NewConnection(domain.ConnectionInfo{
				ID: id, Kind: domain.ConnectionMedia, UserID: user, SessionID: session,
				NodeID: n.ID().String(), DeviceID: device, Mode: p.Demod,
			}, now)
			if err != nil {
				s.logger.WarnContext(ctx, "invalid connection event skipped", slog.Any("error", err))

				return nil
			}

			if _, err := s.repo.Open(ctx, c); err != nil {
				return err
			}
		case err != nil:
			return err
		}

		if c.Info().NodeID != n.ID().String() {
			s.logger.WarnContext(ctx, "connection event for another node's connection ignored", slog.String("node_id", n.ID().String()))

			return nil
		}

		switch ev.Type {
		case rxv1.TypeConnectionClosed:
			c.Close(domain.ParseCloseReason(p.Reason), now)
		default:
			device, err := s.ownedDevice(ctx, n.ID(), p.DeviceID)
			if err != nil {
				return err
			}

			c.Attach(device, p.Demod, now)
		}

		return s.repo.Save(ctx, c)
	}
}

// ownedDevice returns id when it is a device of node, "" otherwise: a node
// cannot attach its listeners to another node's device.
func (s *Presence) ownedDevice(ctx context.Context, node domain.NodeID, id string) (string, error) {
	did, err := shared.NewDeviceID(id)
	if err != nil {
		return "", nil //nolint:nilerr // absent or malformed: no device
	}

	d, err := s.devices.Get(ctx, did)
	if errors.Is(err, domain.ErrDeviceNotFound) {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	if d.Node() != node {
		s.logger.WarnContext(ctx, "connection event names another node's device", slog.String("node_id", node.String()),
			slog.String("device_id", id))

		return "", nil
	}

	return id, nil
}

// Count returns the number of open connections.
func (s *Presence) Count(ctx context.Context) (int, error) { return s.repo.CountOpen(ctx) }

// Listeners returns the number of listeners: open media connections
// (ADR 0018). Events sockets are viewers, not listeners.
func (s *Presence) Listeners(ctx context.Context) (int, error) {
	return s.repo.CountOpenKind(ctx, domain.ConnectionMedia)
}

// List returns the open connections.
func (s *Presence) List(ctx context.Context) ([]*domain.Connection, error) {
	return s.repo.ListOpen(ctx)
}

// Connections retention job (TECHNICAL_SPEC §7.3 "Retention jobs",
// retention.connections).
const (
	JobConnectionsPurge   = "connections.purge"
	ConnectionsPurgeEvery = time.Hour
)

// ConnectionsPurge deletes closed presence rows past their retention.
type ConnectionsPurge struct {
	repo      domain.ConnectionRepository
	retention func() time.Duration
	now       Clock
}

// NewConnectionsPurge returns the job; retention gives the current
// retention.connections.
func NewConnectionsPurge(repo domain.ConnectionRepository, retention func() time.Duration, now Clock) *ConnectionsPurge {
	return &ConnectionsPurge{repo: repo, retention: retention, now: now}
}

// Name implements the jobs scheduler's Job.
func (j *ConnectionsPurge) Name() string { return JobConnectionsPurge }

// Run implements the jobs scheduler's Job: it returns the rows deleted.
func (j *ConnectionsPurge) Run(ctx context.Context) (int64, error) {
	return j.repo.DeleteClosedBefore(ctx, j.now().Add(-j.retention()))
}
