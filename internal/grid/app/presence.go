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

// Presence is the heartbeat-based connection registry (§7.3, GRID-017).
type Presence struct {
	repo    domain.ConnectionRepository
	tracker *Tracker
	timings Timings
	now     Clock
	logger  *slog.Logger
}

// NewPresence returns the service. tracker may be nil when the grid is
// disabled.
func NewPresence(repo domain.ConnectionRepository, tracker *Tracker, timings Timings, now Clock, logger *slog.Logger) *Presence {
	return &Presence{repo: repo, tracker: tracker, timings: timings, now: now, logger: logger}
}

// Open records a new connection (hub events WS, gateway authz).
func (s *Presence) Open(ctx context.Context, info domain.ConnectionInfo) error {
	c, err := domain.NewConnection(info, s.now())
	if err != nil {
		return err
	}

	_, err = s.repo.Open(ctx, c)

	return err
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

	if c.Close(reason, s.now()) {
		return s.repo.Save(ctx, c)
	}

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
	}

	return nil
}

// Reap closes stale rows, the rows of nodes lost for longer than the stale
// delay, and deletes closed rows past the retention.
func (s *Presence) Reap(ctx context.Context) {
	now := s.now()

	if n, err := s.repo.CloseStale(ctx, now.Add(-s.timings.PresenceStale)); err != nil {
		s.logger.ErrorContext(ctx, "close stale connections", slog.Any("error", err))
	} else if n > 0 {
		s.logger.DebugContext(ctx, "stale connections closed", slog.Int64("count", n))
	}

	if s.tracker != nil {
		nodes, err := s.repo.OpenNodes(ctx)
		if err != nil {
			s.logger.ErrorContext(ctx, "list nodes with connections", slog.Any("error", err))
		}

		for _, id := range nodes {
			st, ok := s.tracker.State(id)
			if ok && !st.Connected && !st.DisconnectedAt.IsZero() && now.Sub(st.DisconnectedAt) > s.timings.PresenceStale {
				if _, err := s.repo.CloseNode(ctx, id, domain.CloseNodeLost, now); err != nil {
					s.logger.ErrorContext(ctx, "close connections of a lost node", slog.String("node_id", id.String()), slog.Any("error", err))
				}
			}
		}
	}

	if _, err := s.repo.DeleteClosedBefore(ctx, now.Add(-s.timings.ConnectionsKeep)); err != nil {
		s.logger.ErrorContext(ctx, "delete old connections", slog.Any("error", err))
	}
}

// Run reaps every 15 s (or every stale/3 when shorter) until ctx is done.
func (s *Presence) Run(ctx context.Context) {
	t := time.NewTicker(min(15*time.Second, s.timings.PresenceStale/3))
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reap(ctx)
		}
	}
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
			user, _ := shared.ParseUUID(p.UserID)
			session, _ := shared.ParseUUID(p.SID)

			c, err = domain.NewConnection(domain.ConnectionInfo{
				ID: id, Kind: domain.ConnectionMedia, UserID: user, SessionID: session,
				NodeID: n.ID().String(), DeviceID: validDevice(p.DeviceID), Mode: p.Demod,
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
			c.Attach(validDevice(p.DeviceID), p.Demod, now)
		}

		return s.repo.Save(ctx, c)
	}
}

func validDevice(id string) string {
	if _, err := domain.NewDeviceID(id); err != nil {
		return ""
	}

	return id
}

// Count returns the number of open connections.
func (s *Presence) Count(ctx context.Context) (int, error) { return s.repo.CountOpen(ctx) }

// List returns the open connections.
func (s *Presence) List(ctx context.Context) ([]*domain.Connection, error) {
	return s.repo.ListOpen(ctx)
}
