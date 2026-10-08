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
	// HintStateApplyFailed: the node refused the desired state of some
	// devices, which keep their previous one (§4.9).
	HintStateApplyFailed = "state_apply_failed"
)

// MaxClockOffset is the clock offset above which a node is degraded (§4.5).
const MaxClockOffset = time.Second

// StatusListener is told about node status transitions, after commit
// (§7.3 rule 3). The hub events WS subscribes to it later.
type StatusListener func(ctx context.Context, id domain.NodeID, status domain.Status, hint string)

// Status derives node health from the link state (§4.5, ADR 0008 Q20).
type Status struct {
	nodes     domain.NodeRepository
	tx        Transactor
	tracker   *Tracker
	history   *History
	timings   *timingsCell
	now       Clock
	logger    *slog.Logger
	listeners []StatusListener
}

// NewStatus returns the service.
func NewStatus(nodes domain.NodeRepository, tx Transactor, tracker *Tracker, history *History, timings Timings, now Clock, logger *slog.Logger) *Status {
	return &Status{nodes: nodes, tx: tx, tracker: tracker, history: history, timings: newTimingsCell(timings), now: now, logger: logger}
}

// SetTimings replaces the timings (settings change).
func (s *Status) SetTimings(t Timings) { s.timings.set(t) }

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

	// The node heartbeats at the interval of its channel's ctl.hello, not
	// at a setting changed since (ADR 0018).
	interval := link.HeartbeatInterval
	if interval <= 0 {
		interval = s.timings.get().HeartbeatInterval
	}

	switch {
	case silent >= s.timings.get().OfflineAfter:
		return domain.StatusOffline, HintHeartbeatTimeout
	case silent > 2*interval+interval/2:
		return domain.StatusDegraded, HintMissedHeartbeats
	case abs(link.ClockOffsetMS) > MaxClockOffset.Milliseconds():
		return domain.StatusDegraded, HintClockOffset
	case !link.NTPSynced:
		return domain.StatusDegraded, HintNTPUnsynced
	case len(link.StateErrors) > 0:
		return domain.StatusDegraded, HintStateApplyFailed
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

// Refresh re-evaluates one node and persists a transition. The node is
// read and its status written in one transaction, and only the status
// columns are written: a stale copy never overwrites the runtime state
// recorded by the control channel.
func (s *Status) Refresh(ctx context.Context, id domain.NodeID) {
	var (
		old, status domain.Status
		hint        string
		changed     bool
	)

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		changed = false

		n, err := s.nodes.Get(ctx, id)
		if err != nil {
			return err
		}

		link, known := s.tracker.State(id)
		old = n.Runtime().Status
		status, hint = s.Evaluate(n, link, known, s.now())

		if !n.SetStatus(status, hint) {
			return nil
		}

		changed = true

		return s.nodes.SaveStatus(ctx, id, status, hint)
	})
	if errors.Is(err, domain.ErrNodeNotFound) {
		s.tracker.Forget(id)
		s.history.Forget(id)

		return
	}

	if err != nil {
		if ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "save node status", slog.String("node_id", id.String()), slog.Any("error", err))
		}

		return
	}

	if !changed {
		return
	}

	level := slog.LevelInfo
	if status != domain.StatusOnline {
		level = slog.LevelWarn
	}

	s.logger.LogAttrs(ctx, level, "node status changed", slog.String("node_id", id.String()),
		slog.String("from", string(old)), slog.String("status", string(status)), slog.String("hint", hint))

	for _, l := range s.listeners {
		l(ctx, id, status, hint)
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
		s.Refresh(ctx, n.ID())
	}
}

// Run sweeps until ctx is done, at most every 5 s.
// The period follows the heartbeat interval setting.
func (s *Status) Run(ctx context.Context) {
	every := s.sweepEvery()
	t := time.NewTicker(every)

	defer t.Stop()

	for {
		s.Sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.timings.changes:
		}

		if e := s.sweepEvery(); e != every {
			every = e
			t.Reset(every)
		}
	}
}

func (s *Status) sweepEvery() time.Duration {
	return max(min(5*time.Second, s.timings.get().HeartbeatInterval), 10*time.Millisecond)
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
