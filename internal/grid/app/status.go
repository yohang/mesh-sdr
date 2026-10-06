package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// Status hints (stable codes).
const (
	HintDialFailed       = "dial_failed"
	HintChannelLost      = "channel_lost"
	HintHeartbeatTimeout = "heartbeat_timeout"
	HintMissedHeartbeats = "missed_heartbeats"
	HintClockOffset      = "clock_offset"
	HintNTPUnsynced      = "ntp_unsynced"
)

// MaxClockOffset is the clock offset above which a node is degraded (§4.5).
const MaxClockOffset = time.Second

// StatusListener is told about node status transitions, after commit
// (§7.3 rule 3). The hub events WS subscribes to it later.
type StatusListener func(ctx context.Context, id domain.NodeID, status domain.Status, hint string)

// Status derives node health from the link state (§4.5, ADR 0008 Q20).
type Status struct {
	nodes     domain.NodeRepository
	tracker   *Tracker
	history   *History
	timings   Timings
	now       Clock
	logger    *slog.Logger
	listeners []StatusListener
}

// NewStatus returns the service.
func NewStatus(nodes domain.NodeRepository, tracker *Tracker, history *History, timings Timings, now Clock, logger *slog.Logger) *Status {
	return &Status{nodes: nodes, tracker: tracker, history: history, timings: timings, now: now, logger: logger}
}

// Listen registers a transition listener (composition time only).
func (s *Status) Listen(l StatusListener) { s.listeners = append(s.listeners, l) }

// Evaluate returns the status of n given its link state at now.
func (s *Status) Evaluate(n *domain.Node, link LinkState, known bool, now time.Time) (domain.Status, string) {
	switch {
	case !n.Active():
		return domain.StatusOffline, ""
	case !known || !link.EverWelcomed:
		if link.DialFailures > 0 {
			return domain.StatusUnreachable, HintDialFailed
		}

		return domain.StatusOffline, ""
	case link.Compat.Level == domain.CompatIncompatible:
		return domain.StatusIncompatible, link.Compat.Hint
	case !link.Connected:
		return domain.StatusOffline, HintChannelLost
	}

	last := link.WelcomedAt
	if link.LastHeartbeat.After(last) {
		last = link.LastHeartbeat
	}

	silent := now.Sub(last)
	interval := s.timings.HeartbeatInterval

	switch {
	case silent >= s.timings.OfflineAfter:
		return domain.StatusOffline, HintHeartbeatTimeout
	case silent > 2*interval+interval/2:
		return domain.StatusDegraded, HintMissedHeartbeats
	case abs(link.ClockOffsetMS) > MaxClockOffset.Milliseconds():
		return domain.StatusDegraded, HintClockOffset
	case !link.NTPSynced:
		return domain.StatusDegraded, HintNTPUnsynced
	case link.Compat.Level == domain.CompatOlder:
		return domain.StatusDegraded, link.Compat.Hint
	}

	return domain.StatusOnline, ""
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

// Refresh re-evaluates one node and persists a transition.
func (s *Status) Refresh(ctx context.Context, id domain.NodeID) {
	n, err := s.nodes.Get(ctx, id)
	if errors.Is(err, domain.ErrNodeNotFound) {
		s.tracker.Forget(id)
		s.history.Forget(id)

		return
	}

	if err != nil {
		s.logger.ErrorContext(ctx, "load node for status", slog.String("node_id", id.String()), slog.Any("error", err))

		return
	}

	s.apply(ctx, n)
}

func (s *Status) apply(ctx context.Context, n *domain.Node) {
	link, known := s.tracker.State(n.ID())
	status, hint := s.Evaluate(n, link, known, s.now())
	old := n.Runtime().Status

	if !n.SetStatus(status, hint) {
		return
	}

	if err := s.nodes.SaveRuntime(ctx, n); err != nil {
		s.logger.ErrorContext(ctx, "save node status", slog.String("node_id", n.ID().String()), slog.Any("error", err))

		return
	}

	level := slog.LevelInfo
	if status != domain.StatusOnline {
		level = slog.LevelWarn
	}

	s.logger.LogAttrs(ctx, level, "node status changed", slog.String("node_id", n.ID().String()),
		slog.String("from", string(old)), slog.String("status", string(status)), slog.String("hint", hint))

	for _, l := range s.listeners {
		l(ctx, n.ID(), status, hint)
	}
}

// Sweep re-evaluates every node.
func (s *Status) Sweep(ctx context.Context) {
	nodes, err := s.nodes.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "list nodes for status", slog.Any("error", err))
		}

		return
	}

	for _, n := range nodes {
		s.apply(ctx, n)
	}
}

// Run sweeps until ctx is done, at most every 5 s.
func (s *Status) Run(ctx context.Context) {
	every := min(5*time.Second, s.timings.HeartbeatInterval)
	t := time.NewTicker(every)

	defer t.Stop()

	for {
		s.Sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// HeartbeatHandler applies node.heartbeat events (control ingestion).
func (s *Status) HeartbeatHandler() EventHandler {
	return func(_ context.Context, n *domain.Node, ev Event, now time.Time) error {
		hb, err := decodeEvent[ctl.Heartbeat](ev)
		if err != nil {
			return nil //nolint:nilerr // a malformed heartbeat is skipped, not fatal for the batch
		}

		n.RecordHeartbeat(now, hb.ClockOffsetMS, "", 0)
		s.tracker.Heartbeat(n.ID(), now, hb.ClockOffsetMS, hb.NTPSynced)
		s.history.Add(n.ID(), LoadSample{
			At: now, CPU: hb.CPU, Load1: hb.Load[0], TempC: hb.TempC,
			MemAvailableBytes: hb.Mem.AvailableBytes, MemTotalBytes: hb.Mem.TotalBytes,
		})

		return nil
	}
}

// decodeEvent decodes an event payload, tolerating unknown fields
// (newer-minor nodes, §4.8).
func decodeEvent[T any](ev Event) (T, error) {
	var v T

	err := json.Unmarshal(ev.Payload, &v)

	return v, err
}
