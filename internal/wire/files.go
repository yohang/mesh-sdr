package wire

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/events"
	"github.com/yohang/mesh-sdr/internal/files"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// branding builds the receiver images use case (ADM-004).
func branding(adapter *db.DB, audit audit.Appender) *files.Branding {
	return files.NewBranding(files.BrandingDeps{
		Repo: files.NewFiles(adapter), Tx: adapter, Processor: files.NewProcessor(),
		IDs: shared.NewUUIDv7Generator(), Audit: audit, Now: time.Now,
	})
}

// stationImages tells the shell whether a receiver image is set. An image
// that cannot be read is treated as not set (the Receiver page then links
// no image) and logged at Warn.
type stationImages struct {
	b      *files.Branding
	logger *slog.Logger
}

// HasImage implements shell/app.StationImages.
func (s stationImages) HasImage(ctx context.Context, slot string) bool {
	sl, err := files.ParseSlot(slot)
	if err != nil {
		s.logger.WarnContext(ctx, "unknown receiver image slot", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	f, err := s.b.Current(ctx, sl)

	switch {
	case errors.Is(err, files.ErrImageNotSet):
		return false
	case err != nil:
		s.logger.WarnContext(ctx, "receiver image unavailable", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	return f != nil
}

// The files the nodes send (FIL-005): the control channel events go to the
// files ingest, which the composition root wires to the grid (the node's
// devices and clock offset), the listen policies (who sees a file) and the
// hub events (files.new).

var topicNotifications = events.MustTopic("notifications")

// filesNewEvent is the files.new payload (§6.6).
type filesNewEvent struct {
	FileID           string `json:"file_id"`
	Kind             string `json:"kind"`
	DeviceID         string `json:"device_id,omitempty"`
	Mode             string `json:"mode,omitempty"`
	FrequencyHz      int64  `json:"frequency_hz,omitempty"`
	ReceivedStartUTC string `json:"received_start_utc,omitempty"`
	TS               int64  `json:"ts"`
}

// deviceNames returns the names of the devices of the registry by id (the
// device filter of the Files page).
func deviceNames(repo griddomain.DeviceRepository) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) {
		list, err := repo.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list devices: %w", err)
		}

		out := make(map[string]string, len(list))
		for _, d := range list {
			out[d.ID().String()] = d.Name()
		}

		return out, nil
	}
}

// fileAccess decides who sees the files the nodes sent (ADR 0026: the
// listen policy, no files policy): signed-in users see every file,
// visitors the files of the devices they may listen to.
type fileAccess struct {
	signedIn func(ctx context.Context) bool
	policies *gridapp.ListenPolicies
	logger   *slog.Logger
}

// visibility returns the files the caller of ctx may see.
func (a fileAccess) visibility(ctx context.Context) files.Access {
	if a.signedIn(ctx) {
		return files.Access{All: true}
	}

	return a.anonymous(ctx)
}

// anonymous returns the files a visitor may see; it fails closed.
func (a fileAccess) anonymous(ctx context.Context) files.Access {
	view, err := a.policies.View(ctx)
	if err != nil {
		a.logger.ErrorContext(ctx, "listen policies for the files", slog.Any("error", err))

		return files.Access{}
	}

	return files.Access{Devices: view.Listenable(true)}
}

// published announces a new file on /api/ws (files.new) to the viewers who
// may see it.
func (a fileAccess) published(b events.Publisher) func(ctx context.Context, e files.Entry) {
	return func(ctx context.Context, e files.Entry) {
		vis := a.anonymous(ctx)
		visitors := vis.Allows(e.DeviceID)

		b.Publish(ctx, events.Event{
			Topic: topicNotifications, Type: rxv1.TypeFilesNew.String(),
			Payload: filesNewEvent{
				FileID: e.ID.String(), Kind: string(e.Kind), DeviceID: e.DeviceID, Mode: e.Mode, FrequencyHz: e.FrequencyHz,
				ReceivedStartUTC: e.ReceivedStart.UTC().Format(time.RFC3339Nano), TS: e.CreatedAt.UnixMilli(),
			},
			Audience: func(v events.Viewer) bool { return !v.Anonymous() || visitors },
		})
	}
}

// fileEvents adapts the control channel events file.begin, file.chunk and
// file.end to the files ingest. A payload that cannot be read is logged and
// acknowledged like any refused file.
type fileEvents struct {
	ingest  *files.Ingest
	devices *gridapp.Devices
	logger  *slog.Logger
}

// register adds the handlers to the control channel.
func (f fileEvents) register(c *gridapp.Control) {
	c.Handle(rxv1.TypeFileBegin, gridapp.On(f.logger, f.begin))
	c.Handle(rxv1.TypeFileChunk, gridapp.On(f.logger, f.chunk))
	c.Handle(rxv1.TypeFileEnd, gridapp.On(f.logger, f.end))
	c.OnApplied(func(ctx context.Context, id griddomain.NodeID, types []rxv1.MessageType) {
		if slices.Contains(types, rxv1.TypeFileEnd) {
			// The batch is committed: a closing channel must not cut the
			// completion short.
			f.ingest.Finalize(context.WithoutCancel(ctx), id.String())
		}
	})
}

func (f fileEvents) refused(ctx context.Context, n *griddomain.Node, t rxv1.MessageType, reason string) error {
	f.logger.WarnContext(ctx, "file event from a node refused", slog.String("node_id", n.ID().String()),
		slog.String("type", t.String()), slog.String("reason", reason))

	return nil
}

func (f fileEvents) begin(ctx context.Context, n *griddomain.Node, p ctl.FileBegin, now time.Time) error {
	in := files.Incoming{
		Node: n.ID().String(), Kind: files.Kind(p.Kind), MIME: files.MIMEType(p.MIME), Size: p.Size, Mode: p.Mode,
		FrequencyHz: p.FrequencyHz, Metadata: p.Metadata,
	}

	var errs []error

	parse := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error

	in.ID, err = shared.ParseUUID(p.FileID)
	parse(err)

	in.SHA256, err = hex.DecodeString(p.SHA256)
	parse(err)

	in.DeviceID, err = shared.NewDeviceID(p.DeviceID)
	parse(err)

	in.ReceivedStart, err = time.Parse(time.RFC3339Nano, p.ReceivedStartUTC)
	parse(err)

	if p.ReceivedEndUTC != "" {
		in.ReceivedEnd, err = time.Parse(time.RFC3339Nano, p.ReceivedEndUTC)
		parse(err)
	}

	for _, u := range []struct {
		s  string
		to *shared.UUID
	}{{p.PresetID, &in.PresetID}, {p.DecoderSessionID, &in.DecoderSessionID}} {
		if u.s != "" {
			*u.to, err = shared.ParseUUID(u.s)
			parse(err)
		}
	}

	if len(errs) > 0 {
		return f.refused(ctx, n, rxv1.TypeFileBegin, errors.Join(errs...).Error())
	}

	owned, err := f.devices.OwnedBy(ctx, n.ID(), in.DeviceID)
	if err != nil {
		return err
	}

	if !owned {
		return f.refused(ctx, n, rxv1.TypeFileBegin, "device "+strconv.Quote(p.DeviceID)+" is not a device of the node")
	}

	return f.ingest.Begin(ctx, in, n.Runtime().ClockOffsetMS, now)
}

func (f fileEvents) chunk(ctx context.Context, n *griddomain.Node, p ctl.FileChunk, now time.Time) error {
	id, err := shared.ParseUUID(p.FileID)
	if err != nil {
		return f.refused(ctx, n, rxv1.TypeFileChunk, "invalid file id")
	}

	var data []byte

	// An undecodable or oversized chunk reaches the ingest empty, which
	// refuses the file.
	if len(p.DataB64) <= base64.StdEncoding.EncodedLen(files.MaxWireChunk) {
		if d, err := base64.StdEncoding.DecodeString(p.DataB64); err == nil {
			data = d
		}
	}

	return f.ingest.Chunk(ctx, n.ID().String(), id, p.Offset, data, now)
}

func (f fileEvents) end(ctx context.Context, n *griddomain.Node, p ctl.FileEnd, _ time.Time) error {
	id, err := shared.ParseUUID(p.FileID)
	if err != nil {
		return f.refused(ctx, n, rxv1.TypeFileEnd, "invalid file id")
	}

	return f.ingest.End(ctx, n.ID().String(), id)
}
