package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/db"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Ingest stores the files the nodes send over the control channel
// (FIL-005, TECHNICAL_SPEC §4.4 file.begin/chunk/end, §7.3 "File blobs in
// the DB"). Begin, Chunk and End run inside the transaction that ingests a
// batch of node events: a file is stored receiving, its content appended
// in chunks of at most 1 MiB, and its size and SHA-256 checked at the end.
// After the commit, Finalize validates the content (images are decoded and
// re-encoded, which drops their metadata, and get a thumbnail; text must be
// UTF-8), marks the file complete, applies the retention and announces it.
//
// Everything a node sends is untrusted: a file that breaks a rule is
// deleted and logged, never an error, so that the node's events are still
// acknowledged (the node then deletes its copy).
type Ingest struct {
	repo      *Files
	tx        *db.DB
	proc      *Processor
	retention *Retention
	published func(ctx context.Context, e Entry)
	logger    *slog.Logger

	mu    sync.Mutex
	ended []shared.UUID
}

// IngestDeps are the dependencies of Ingest.
type IngestDeps struct {
	Repo      *Files
	Tx        *db.DB
	Processor *Processor
	// Retention is applied after each new file; nil applies none.
	Retention *Retention
	// Published is told each new complete file (files.new); nil tells
	// nobody.
	Published func(ctx context.Context, e Entry)
	Logger    *slog.Logger
}

// NewIngest returns the use case.
func NewIngest(d IngestDeps) *Ingest {
	return &Ingest{repo: d.Repo, tx: d.Tx, proc: d.Processor, retention: d.Retention, published: d.Published, logger: d.Logger}
}

// Begin stores a file a node announces (file.begin), receiving.
// clockOffsetMS is the hub − node clock offset the node last reported (nil
// when unknown): beyond ClockSkewLimit the file records it (FIL-008).
func (i *Ingest) Begin(ctx context.Context, in Incoming, clockOffsetMS *int64, now time.Time) error {
	if err := in.Check(); err != nil {
		i.logger.WarnContext(ctx, "file from a node refused", slog.String("node_id", in.Node),
			slog.String("file_id", in.ID.String()), slog.Any("error", err))

		return nil
	}

	exists, err := i.repo.exists(ctx, in.ID)
	if err != nil || exists {
		return err
	}

	var skew int64
	if clockOffsetMS != nil {
		skew = *clockOffsetMS
	}

	if skew > ClockSkewLimit.Milliseconds() || skew < -ClockSkewLimit.Milliseconds() {
		i.logger.WarnContext(ctx, "file from a node with a skewed clock: its reception times are kept as sent",
			slog.String("node_id", in.Node), slog.String("file_id", in.ID.String()), slog.Int64("clock_skew_ms", skew))
	}

	metadata, err := in.metadataWithSkew(skew)
	if err != nil {
		i.logger.WarnContext(ctx, "file from a node refused", slog.String("node_id", in.Node),
			slog.String("file_id", in.ID.String()), slog.Any("error", err))

		return nil
	}

	i.logger.DebugContext(ctx, "file announced", slog.String("node_id", in.Node), slog.String("file_id", in.ID.String()),
		slog.String("kind", string(in.Kind)), slog.Int64("size", in.Size))

	return i.repo.addReceiving(ctx, in, metadata, now)
}

// open returns a file of node being received; ok is false when there is
// none (refused at its beginning, or already refused).
func (i *Ingest) open(ctx context.Context, node string, id shared.UUID) (receiving, bool, error) {
	f, err := i.repo.receiving(ctx, id)

	switch {
	case errors.Is(err, ErrFileNotFound):
		i.logger.DebugContext(ctx, "content of an unknown file ignored", slog.String("node_id", node), slog.String("file_id", id.String()))

		return receiving{}, false, nil
	case err != nil:
		return receiving{}, false, err
	case f.nodeID != node:
		i.logger.WarnContext(ctx, "content of another node's file ignored", slog.String("node_id", node), slog.String("file_id", id.String()))

		return receiving{}, false, nil
	}

	return f, true, nil
}

// refuse deletes a file that cannot complete.
func (i *Ingest) refuse(ctx context.Context, node string, id shared.UUID, reason string) error {
	i.logger.WarnContext(ctx, "file from a node refused", slog.String("node_id", node), slog.String("file_id", id.String()),
		slog.String("reason", reason))

	_, err := i.repo.remove(ctx, id)

	return err
}

// Chunk appends content to a file being received (file.chunk). The chunks
// must follow each other without gap nor overlap.
func (i *Ingest) Chunk(ctx context.Context, node string, id shared.UUID, offset int64, data []byte) error {
	f, ok, err := i.open(ctx, node, id)
	if err != nil || !ok {
		return err
	}

	if len(data) == 0 || len(data) > MaxWireChunk {
		return i.refuse(ctx, node, id, fmt.Sprintf("a chunk has %d bytes", len(data)))
	}

	got, err := i.repo.received(ctx, id)
	if err != nil {
		return err
	}

	if offset != got || got+int64(len(data)) > f.size {
		return i.refuse(ctx, node, id, fmt.Sprintf("chunk at %d of %d bytes after %d of %d bytes", offset, len(data), got, f.size))
	}

	return i.repo.appendContent(ctx, id, data)
}

// End checks the size and digest of a received file (file.end) and queues
// it for Finalize.
func (i *Ingest) End(ctx context.Context, node string, id shared.UUID) error {
	f, ok, err := i.open(ctx, node, id)
	if err != nil || !ok {
		return err
	}

	data, err := i.repo.contentOf(ctx, id)
	if err != nil {
		return err
	}

	if int64(len(data)) != f.size || !bytes.Equal(sha256Sum(data), f.sum) {
		return i.refuse(ctx, node, id, fmt.Sprintf("%d bytes received of %d, or a SHA-256 mismatch", len(data), f.size))
	}

	i.mu.Lock()
	i.ended = append(i.ended, id)
	i.mu.Unlock()

	return nil
}

// Finalize completes the files whose content arrived, once the ingestion
// transaction committed. It is the boundary of the post-commit work: it
// logs its failures; a file it cannot complete is deleted after
// IncompleteAfter by the retention job.
func (i *Ingest) Finalize(ctx context.Context) {
	i.mu.Lock()
	ids := i.ended
	i.ended = nil
	i.mu.Unlock()

	for _, id := range ids {
		if err := i.finalize(ctx, id); err != nil {
			i.logger.ErrorContext(ctx, "complete a file from a node", slog.String("file_id", id.String()), slog.Any("error", err))
		}
	}
}

func (i *Ingest) finalize(ctx context.Context, id shared.UUID) error {
	f, err := i.repo.receiving(ctx, id)
	if errors.Is(err, ErrFileNotFound) {
		return nil // the batch was rolled back, or the file purged
	}

	if err != nil {
		return err
	}

	data, err := i.repo.contentOf(ctx, id)
	if err != nil {
		return err
	}

	if int64(len(data)) != f.size || !bytes.Equal(sha256Sum(data), f.sum) {
		return nil // the end of the file was not committed
	}

	c, reason, err := i.validate(ctx, f, data)
	if err != nil {
		return err
	}

	if reason != "" {
		return i.refuse(ctx, f.nodeID, id, reason)
	}

	var done bool

	err = i.tx.WithinTx(ctx, func(ctx context.Context) error {
		done, err = i.repo.complete(ctx, id, c)

		return err
	})
	if err != nil || !done {
		return err
	}

	e, err := i.repo.Entry(ctx, id)
	if err != nil {
		return err
	}

	i.logger.InfoContext(ctx, "file received", slog.String("node_id", e.NodeID), slog.String("file_id", id.String()),
		slog.String("kind", string(e.Kind)), slog.String("device_id", e.DeviceID), slog.Int64("size", e.Size))

	if i.retention != nil {
		if _, err := i.retention.Apply(ctx); err != nil {
			i.logger.ErrorContext(ctx, "apply the files retention", slog.Any("error", err))
		}
	}

	if i.published != nil {
		i.published(ctx, e)
	}

	return nil
}

// validate checks the content of a file against its type and returns the
// content to store, or the reason it is refused.
func (i *Ingest) validate(ctx context.Context, f receiving, data []byte) (completed, string, error) {
	switch f.mime {
	case MIMEPNG:
		if t, err := Sniff(data); err != nil || t != MIMEPNG {
			return completed{}, "the content is not a PNG image", nil
		}

		img, thumb, err := i.proc.ReencodeWithThumbnail(ctx, data, MIMEPNG, MaxProducedPixels, ThumbnailSide)

		var de *shared.Error

		switch {
		case errors.As(err, &de):
			return completed{}, "invalid image: " + de.Message(), nil
		case err != nil:
			return completed{}, "", err
		case int64(len(img.Data)) > MaxSize(f.kind):
			return completed{}, "the re-encoded image exceeds the size cap", nil
		}

		return completed{content: img.Data, width: img.Width, height: img.Height, thumbnail: thumb.Data}, "", nil
	case MIMEText:
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return completed{}, "the text is not UTF-8", nil
		}

		return completed{content: data}, "", nil
	default:
		return completed{}, "unsupported media type " + string(f.mime), nil
	}
}
