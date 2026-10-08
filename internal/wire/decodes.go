package wire

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/decodes"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/settings"
)

// decodesDeps are the hub parts the decodes module uses.
type decodesDeps struct {
	adapter  *db.DB
	grid     *hubGrid
	broker   events.Publisher
	policies *policyCache
	identity interface {
		Principal(ctx context.Context) identitydomain.Principal
		Authorize(ctx context.Context, role identitydomain.Role) error
	}
	// features gives the enabled devices (set once built).
	features func() *gridapp.Features
	store    *settings.Store
	render   decodes.Renderer
	now      func() time.Time
	logger   *slog.Logger
}

// newDecodes builds the decoded messages module (DEC-047): the decode.batch
// handler of the control channels, decode.new on /api/ws after each
// commit, and the Decodes page, whose rows follow the listen policy.
func newDecodes(d decodesDeps) *decodes.Module {
	logger := component(d.logger, "decodes.app")

	m := decodes.New(decodes.Deps{
		DB:        d.adapter,
		Visible:   visibleDevices(d),
		SignedIn:  func(ctx context.Context) bool { return d.identity.Authorize(ctx, identitydomain.RoleListener) == nil },
		Retention: func() time.Duration { return d.store.Duration("retention.decoded_messages.max_age") },
		MaxRows:   func() int { return d.store.Int("retention.decoded_messages.max_rows") },
		Published: decodeNew(d.broker, logger),
		Render:    d.render, Now: d.now, Logger: logger,
	})

	if c := d.grid.control; c != nil {
		c.Handle(rxv1.TypeDecodeBatch, func(ctx context.Context, n *griddomain.Node, ev gridapp.Event, now time.Time) error {
			return m.Ingest(ctx, n.ID().String(), ev.Payload, now)
		})
		c.OnApplied(func(ctx context.Context, id griddomain.NodeID, types []rxv1.MessageType) {
			if slices.Contains(types, rxv1.TypeDecodeBatch) {
				m.Flush(ctx, id.String())
			}
		})
		c.OnFailed(func(id griddomain.NodeID) { m.Discard(id.String()) })
	}

	return m
}

// visibleDevices returns the enabled devices the visitor may listen to
// (ADR 0026: the Decodes page follows the listen policy).
func visibleDevices(d decodesDeps) func(ctx context.Context) ([]decodes.Device, error) {
	return func(ctx context.Context) ([]decodes.Device, error) {
		summary, err := d.features().Summary(ctx)
		if err != nil {
			return nil, err
		}

		snap, err := d.policies.get(ctx)
		if err != nil {
			return nil, err
		}

		p := d.identity.Principal(ctx)
		out := []decodes.Device{}

		for _, dev := range summary.Devices {
			if id := dev.ID.String(); snap.canListen(p, id) {
				out = append(out, decodes.Device{ID: id, Name: dev.Name})
			}
		}

		return out, nil
	}
}

// decodeNewEvent is the decode.new payload (§6.6); text is untrusted RF
// text, inserted with textContent.
type decodeNewEvent struct {
	ID       int64  `json:"id"`
	DeviceID string `json:"device_id"`
	NodeID   string `json:"node_id"`
	Mode     string `json:"mode"`
	FreqHz   int64  `json:"freq_hz,omitempty"`
	TS       int64  `json:"ts"`
	Source   string `json:"source"`
	Text     string `json:"text,omitempty"`
	Schema   string `json:"schema"`
	Payload  any    `json:"payload"`
}

// decodeNew publishes a stored message on decodes:device=<id>: the topic
// requires listen permission on the device.
func decodeNew(b events.Publisher, logger *slog.Logger) func(context.Context, decodes.Message) {
	return func(ctx context.Context, m decodes.Message) {
		topic, err := events.ParseTopic(string(events.KindDecodes) + ":device=" + m.DeviceID)
		if err != nil {
			logger.WarnContext(ctx, "decode.new topic", slog.String("device_id", m.DeviceID), slog.Any("error", err))

			return
		}

		source := "listener"
		if m.Origin == "service" {
			source = "background"
		}

		b.Publish(ctx, events.Event{Topic: topic, Type: rxv1.TypeDecodeNew.String(), Payload: decodeNewEvent{
			ID: m.ID, DeviceID: m.DeviceID, NodeID: m.NodeID, Mode: m.Mode, FreqHz: m.FreqHz, TS: m.DecodedAt.UnixMilli(),
			Source: source, Text: m.Text, Schema: m.Schema, Payload: m.Payload,
		}})
	}
}
