package wire

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/decodes"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/mapfeatures"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	radiodomain "github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/settings"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// decodesDeps are the hub parts the decodes module uses.
type decodesDeps struct {
	adapter  *db.DB
	grid     *hubGrid
	broker   events.Publisher
	identity interface {
		Principal(ctx context.Context) identitydomain.Principal
		Authorize(ctx context.Context, role identitydomain.Role) error
	}
	// features gives the enabled devices.
	features *gridapp.Features
	// mapf projects the stored messages onto the map (nil: none).
	mapf   *mapfeatures.Module
	store  *settings.Store
	render *render.Renderer
	now    func() time.Time
	logger *slog.Logger
}

// newDecodes builds the decoded messages module (DEC-047): the decode.batch
// handler of the control channels, decode.new on /api/ws after each
// commit, and the Decodes page, whose rows follow the listen policy.
func newDecodes(d decodesDeps) *decodes.Module {
	logger := component(d.logger, "decodes.module")

	var stored func(ctx context.Context, m decodes.Message) error
	if d.mapf != nil {
		stored = func(ctx context.Context, m decodes.Message) error { return d.mapf.Ingest(ctx, mapDecode(m)) }
	}

	m := decodes.New(decodes.Deps{
		DB:         d.adapter,
		Visible:    visibleDevices(d),
		SignedIn:   func(ctx context.Context) bool { return d.identity.Authorize(ctx, identitydomain.RoleListener) == nil },
		Retention:  func() time.Duration { return d.store.Duration("retention.decoded_messages.max_age") },
		MaxRows:    func() int { return d.store.Int("retention.decoded_messages.max_rows") },
		Published:  decodeNew(d.broker, logger),
		Stored:     stored,
		DeviceNode: deviceNode(d.grid.deviceRepo),
		Modes:      catalogueModes(),
		Dedup:      dedupOf,
		Render:     d.render, Now: d.now, Logger: logger,
	})

	if c := d.grid.control; c != nil {
		c.Handle(rxv1.TypeDecodeBatch, func(ctx context.Context, n *griddomain.Node, ev gridapp.Event, now time.Time) error {
			return m.Ingest(ctx, n.ID().String(), ev.Payload, now)
		})
		c.OnApplied(func(ctx context.Context, id griddomain.NodeID, types []rxv1.MessageType) {
			if slices.Contains(types, rxv1.TypeDecodeBatch) {
				m.Flush(ctx, id.String())

				if d.mapf != nil {
					d.mapf.Flush(ctx, id.String())
				}
			}
		})
		c.OnFailed(func(id griddomain.NodeID) {
			m.Discard(id.String())

			if d.mapf != nil {
				d.mapf.Discard(id.String())
			}
		})
	}

	return m
}

// deviceNode returns the node of a device of the registry.
func deviceNode(repo griddomain.DeviceRepository) func(ctx context.Context, device string) (string, bool, error) {
	return func(ctx context.Context, device string) (string, bool, error) {
		id, err := shared.NewDeviceID(device)
		if err != nil {
			return "", false, nil
		}

		dev, err := repo.Get(ctx, id)

		switch {
		case errors.Is(err, griddomain.ErrDeviceNotFound):
			return "", false, nil
		case err != nil:
			return "", false, err
		}

		return dev.Node().String(), true, nil
	}
}

// catalogueModes are the digital modes of the node catalogue (the mode
// filter of the Decodes page).
func catalogueModes() []decodes.Mode {
	var out []decodes.Mode

	for _, m := range radiodomain.DigitalModes() {
		out = append(out, decodes.Mode{ID: m.Name, Label: m.Label})
	}

	return out
}

// dedupOf is the duplicate key rounding of a mode of the catalogue (0 for
// an unknown mode: the module's default).
func dedupOf(mode string) (int64, time.Duration) {
	for _, m := range radiodomain.DigitalModes() {
		if m.Name == mode {
			return m.DedupStep, m.DedupBucket()
		}
	}

	return 0, 0
}

// visibleDevices returns the enabled devices the visitor may listen to
// (ADR 0026: the Decodes page follows the listen policy).
func visibleDevices(d decodesDeps) func(ctx context.Context) ([]decodes.Device, error) {
	return func(ctx context.Context) ([]decodes.Device, error) {
		summary, err := d.features.Summary(ctx)
		if err != nil {
			return nil, err
		}

		anonymous := d.identity.Principal(ctx).IsAnonymous()
		out := []decodes.Device{}

		for _, dev := range summary.Devices {
			if dev.CanListen(anonymous) {
				out = append(out, decodes.Device{ID: dev.ID.String(), Name: dev.Name})
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
