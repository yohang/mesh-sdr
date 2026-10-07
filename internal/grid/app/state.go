package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// DesiredStates builds the desired state of a node's devices (§4.4
// ctl.state.apply), without its revision.
type DesiredStates interface {
	Desired(ctx context.Context, node domain.NodeID) (ctl.StateApply, error)
}

// StateSender sends a desired state on a node's open channel
// (domain.ErrNodeUnavailable when none is open).
type StateSender interface {
	SendState(ctx context.Context, id domain.NodeID, st ctl.StateApply) error
}

// MaxStateBytes bounds an encoded desired state: the node reads at most
// 64 KiB per control message (§6.9), envelope included.
const MaxStateBytes = rxv1.MaxInboundControlTextBytes - 2<<10

// ErrStateTooLarge is a desired state that does not fit a control message.
var ErrStateTooLarge = errors.New("desired state too large for a control message")

// States pushes the desired state of the nodes (ADR 0020): after a welcome
// whose last applied revision differs, after every change and hourly
// (sliding timeline). A revision is derived from the content, so an
// unchanged state is never sent twice on a channel.
type States struct {
	desired DesiredStates
	sender  StateSender
	tracker *Tracker
	logger  *slog.Logger

	mu sync.Mutex
}

// NewStates returns the service.
func NewStates(desired DesiredStates, sender StateSender, tracker *Tracker, logger *slog.Logger) *States {
	return &States{desired: desired, sender: sender, tracker: tracker, logger: logger}
}

// SetSender sets the channel sender (composition time only: the control
// manager is built after the service).
func (s *States) SetSender(sender StateSender) { s.sender = sender }

// Revision derives the revision of a desired state from its content: a
// positive 63-bit prefix of the SHA-256 of its JSON without revision.
func Revision(st ctl.StateApply) (int64, error) {
	st.Revision = 0

	b, err := json.Marshal(st)
	if err != nil {
		return 0, fmt.Errorf("encode desired state: %w", err)
	}

	sum := sha256.Sum256(b)
	rev := int64(binary.BigEndian.Uint64(sum[:8]) >> 1)

	if rev == 0 {
		rev = 1
	}

	return rev, nil
}

// build returns the desired state of id with its revision.
func (s *States) build(ctx context.Context, id domain.NodeID) (ctl.StateApply, error) {
	st, err := s.desired.Desired(ctx, id)
	if err != nil {
		return ctl.StateApply{}, fmt.Errorf("desired state of %s: %w", id, err)
	}

	if st.Revision, err = Revision(st); err != nil {
		return ctl.StateApply{}, err
	}

	b, err := json.Marshal(st)
	if err != nil {
		return ctl.StateApply{}, fmt.Errorf("encode desired state: %w", err)
	}

	if len(b) > MaxStateBytes {
		return ctl.StateApply{}, fmt.Errorf("node %s: %w (%d bytes)", id, ErrStateTooLarge, len(b))
	}

	return st, nil
}

// Welcomed pushes the desired state on a new channel, unless the node
// already applied this revision (§4.4 ctl.welcome).
func (s *States) Welcomed(ctx context.Context, id domain.NodeID, lastApplied int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.build(ctx, id)
	if err != nil {
		return err
	}

	if st.Revision == lastApplied {
		s.tracker.StatePushed(id, st.Revision)
		s.tracker.StateApplied(id, st.Revision, nil)

		return nil
	}

	return s.send(ctx, id, st)
}

// Publish pushes the desired state of id when it changed since the last
// push on its channel. A node without an open channel, or an incompatible
// one, is skipped: it gets the state when it connects.
func (s *States) Publish(ctx context.Context, id domain.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	link, ok := s.tracker.State(id)
	if !ok || !link.Connected || link.Compat.Level == domain.CompatIncompatible {
		return nil
	}

	st, err := s.build(ctx, id)
	if err != nil {
		return err
	}

	if st.Revision == link.StateRevision {
		return nil
	}

	return s.send(ctx, id, st)
}

func (s *States) send(ctx context.Context, id domain.NodeID, st ctl.StateApply) error {
	if err := s.sender.SendState(ctx, id, st); err != nil {
		if errors.Is(err, domain.ErrNodeUnavailable) {
			return nil
		}

		return fmt.Errorf("send desired state to %s: %w", id, err)
	}

	s.tracker.StatePushed(id, st.Revision)
	s.logger.DebugContext(ctx, "desired state pushed", slog.String("node_id", id.String()), slog.Int64("revision", st.Revision),
		slog.Int("devices", len(st.Devices)))

	return nil
}

// PublishAll pushes the desired state of every connected node that needs
// it, and returns the number of nodes it was sent to. Errors are logged
// per node.
func (s *States) PublishAll(ctx context.Context) int {
	sent := 0

	for _, id := range s.tracker.Connected() {
		before, _ := s.tracker.State(id)

		if err := s.Publish(ctx, id); err != nil {
			s.logger.ErrorContext(ctx, "push desired state", slog.String("node_id", id.String()), slog.Any("error", err))

			continue
		}

		if after, _ := s.tracker.State(id); after.StateRevision != before.StateRevision {
			sent++
		}
	}

	return sent
}

// Applied records a node's ctl.state.applied.
func (s *States) Applied(ctx context.Context, id domain.NodeID, a ctl.StateApplied) {
	s.tracker.StateApplied(id, a.Revision, a.Errors)

	for _, e := range a.Errors {
		s.logger.WarnContext(ctx, "node refused the desired state of a device", slog.String("node_id", id.String()),
			slog.Int64("revision", a.Revision), slog.String("device_id", e.DeviceID), slog.String("code", e.Code),
			slog.String("reason", e.Reason))
	}
}
