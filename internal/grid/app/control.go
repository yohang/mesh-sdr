package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Event is one numbered node → hub event received on a control channel.
type Event struct {
	Seq     int64
	Type    rxv1.MessageType
	Payload json.RawMessage
}

// EventHandler applies one event inside the ingestion transaction. It may
// change the node's runtime state, which is saved after the batch.
type EventHandler func(ctx context.Context, n *domain.Node, ev Event, now time.Time) error

// BootHandler runs, inside the welcome transaction, when a node reports a
// boot id different from the last one (§4.9 "Node restarts").
type BootHandler func(ctx context.Context, id domain.NodeID, now time.Time) error

// Decision is the hub's answer to a ctl.welcome.
type Decision struct {
	Compat domain.Compatibility
	// Restricted channels only accept welcome, capabilities and
	// heartbeats, and receive no state (§4.8).
	Restricted bool
}

// incompatibleTypes are the only events accepted from an incompatible node.
var incompatibleTypes = []rxv1.MessageType{rxv1.TypeNodeCapabilities, rxv1.TypeNodeHeartbeat}

// Control is the hub side of the control channels: welcome handling,
// idempotent event ingestion and channel lifecycle (§4.4, §4.9).
type Control struct {
	nodes       domain.NodeRepository
	revocations domain.RevocationRepository
	cursors     domain.EventCursorRepository
	tx          Transactor
	audit       Auditor
	now         Clock
	logger      *slog.Logger
	hubVersion  string

	handlers map[rxv1.MessageType]EventHandler
	onBoot   []BootHandler
	onLink   []func(ctx context.Context, id domain.NodeID)
	links    *Tracker

	mu     sync.Mutex
	warned map[rxv1.MessageType]bool
}

// NewControl returns the service.
func NewControl(nodes domain.NodeRepository, revocations domain.RevocationRepository, cursors domain.EventCursorRepository,
	tx Transactor, audit Auditor, tracker *Tracker, hubVersion string, now Clock, logger *slog.Logger,
) *Control {
	return &Control{
		nodes: nodes, revocations: revocations, cursors: cursors, tx: tx, audit: audit, now: now, logger: logger, hubVersion: hubVersion,
		handlers: map[rxv1.MessageType]EventHandler{}, links: tracker, warned: map[rxv1.MessageType]bool{},
	}
}

// Handle registers the handler of an event type (composition time only).
func (c *Control) Handle(t rxv1.MessageType, h EventHandler) { c.handlers[t] = h }

// OnBoot registers a node-restart handler (composition time only).
func (c *Control) OnBoot(h BootHandler) { c.onBoot = append(c.onBoot, h) }

// OnLinkChange registers a callback run after a welcome, a disconnection,
// a failed dial or an ingested batch (composition time only).
func (c *Control) OnLinkChange(f func(ctx context.Context, id domain.NodeID)) {
	c.onLink = append(c.onLink, f)
}

func (c *Control) linkChanged(ctx context.Context, id domain.NodeID) {
	for _, f := range c.onLink {
		f(ctx, id)
	}
}

// HubVersion returns the product version announced in ctl.hello.
func (c *Control) HubVersion() string { return c.hubVersion }

// Welcome records a node's ctl.welcome and decides how the hub talks to it.
func (c *Control) Welcome(ctx context.Context, id domain.NodeID, w ctl.Welcome) (Decision, error) {
	boot, err := shared.ParseUUID(w.BootID)
	if err != nil {
		return Decision{}, fmt.Errorf("node %s welcome: boot_id: %w", id, err)
	}

	compat := domain.CheckCompatibility(c.hubVersion, w.Version, w.Protocols)
	now := c.now()

	err = c.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := c.nodes.Get(ctx, id)
		if err != nil {
			return err
		}

		restarted := n.Runtime().BootID != boot

		n.RecordWelcome(boot, w.Version, rxv1.ControlSubprotocol)

		if err := c.nodes.SaveRuntime(ctx, n); err != nil {
			return err
		}

		if !restarted {
			return nil
		}

		if err := c.cursors.Forget(ctx, id, boot); err != nil {
			return err
		}

		for _, h := range c.onBoot {
			if err := h(ctx, id, now); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return Decision{}, fmt.Errorf("node %s welcome: %w", id, err)
	}

	d := Decision{Compat: compat, Restricted: compat.Level == domain.CompatIncompatible}

	c.links.Welcomed(id, boot, compat, now)
	c.linkChanged(ctx, id)
	c.logger.InfoContext(ctx, "node connected", slog.String("node_id", id.String()), slog.String("version", w.Version),
		slog.String("boot_id", boot.String()), slog.String("compat", string(compat.Level)), slog.String("hint", compat.Hint))

	return d, nil
}

// Apply ingests a batch of events of (node, boot) in one transaction,
// skipping events already applied, and returns the seq to acknowledge.
func (c *Control) Apply(ctx context.Context, id domain.NodeID, boot shared.UUID, restricted bool, events []Event) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}

	now := c.now()

	var upto int64

	err := c.tx.WithinTx(ctx, func(ctx context.Context) error {
		upto = 0

		last, err := c.cursors.Last(ctx, id, boot)
		if err != nil {
			return err
		}

		n, err := c.nodes.Get(ctx, id)
		if err != nil {
			return err
		}

		applied := last

		for _, ev := range events {
			upto = max(upto, ev.Seq)

			if ev.Seq <= applied {
				continue
			}

			applied = ev.Seq

			if restricted && !slices.Contains(incompatibleTypes, ev.Type) {
				continue
			}

			h, ok := c.handlers[ev.Type]
			if !ok {
				c.warnOnce(ctx, ev.Type)

				continue
			}

			if err := h(ctx, n, ev, now); err != nil {
				return fmt.Errorf("apply %s seq %d: %w", ev.Type, ev.Seq, err)
			}
		}

		if applied == last {
			return nil
		}

		if err := c.nodes.SaveRuntime(ctx, n); err != nil {
			return err
		}

		return c.cursors.Advance(ctx, id, boot, applied, now)
	})
	if err != nil {
		return 0, fmt.Errorf("node %s events: %w", id, err)
	}

	c.linkChanged(ctx, id)

	return upto, nil
}

func (c *Control) warnOnce(ctx context.Context, t rxv1.MessageType) {
	c.mu.Lock()
	seen := c.warned[t]
	c.warned[t] = true
	c.mu.Unlock()

	if !seen {
		c.logger.WarnContext(ctx, "node event type not implemented yet: acknowledged and ignored", slog.String("type", string(t)))
	}
}

// Disconnected records the end of a node's control channel.
func (c *Control) Disconnected(ctx context.Context, id domain.NodeID, cause error) {
	c.links.Disconnected(id, c.now())
	c.linkChanged(ctx, id)
	c.logger.InfoContext(ctx, "node disconnected", slog.String("node_id", id.String()), slog.Any("cause", cause))
}

// DialFailed records a failed dial to a node.
func (c *Control) DialFailed(ctx context.Context, id domain.NodeID, cause error) {
	c.links.DialFailed(id, c.now())
	c.linkChanged(ctx, id)
	c.logger.DebugContext(ctx, "node dial failed", slog.String("node_id", id.String()), slog.Any("error", cause))
}

// ProposeRenewal records a renewed certificate before it is sent to the
// node: from then on the hub accepts it as well as the current one.
func (c *Control) ProposeRenewal(ctx context.Context, id domain.NodeID, cert domain.CertInfo) error {
	err := c.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := c.nodes.Get(ctx, id)
		if err != nil {
			return err
		}

		v := n.Version()

		// A previous renewal never confirmed is replaced: revoke it, it was
		// signed and may have reached the node.
		if p := n.PendingCertificate(); p.Serial() != cert.Serial() {
			if err := revoke(ctx, c.revocations, p, id, "superseded", c.now()); err != nil {
				return err
			}
		}

		if err := n.ProposeCertificate(cert, c.now()); err != nil {
			return err
		}

		return c.nodes.Save(ctx, n, v)
	})
	if err != nil {
		return fmt.Errorf("propose renewed certificate of %s: %w", id, err)
	}

	return nil
}

// RecordRenewal promotes the pending certificate of serial once the node
// uses it (acknowledgement or a connection presenting it), and revokes the
// replaced certificate.
func (c *Control) RecordRenewal(ctx context.Context, id domain.NodeID, serial string) error {
	now := c.now()

	err := c.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := c.nodes.Get(ctx, id)
		if err != nil {
			return err
		}

		v := n.Version()

		old, err := n.PromoteCertificate(serial, now)
		if err != nil {
			return err
		}

		if !old.IsZero() {
			r, err := domain.NewRevokedCertificate(old, id, "renewed", now)
			if err != nil {
				return err
			}

			if err := c.revocations.Add(ctx, r); err != nil {
				return err
			}
		}

		return c.nodes.Save(ctx, n, v)
	})
	if err != nil {
		return fmt.Errorf("record renewed certificate of %s: %w", id, err)
	}

	c.audit.Record(ctx, AuditRecord{ActorKind: ActorSystem, Action: "node.cert.renew", Target: id.String(), Result: ResultOK,
		Detail: map[string]string{"cert_serial": serial}})

	return nil
}
