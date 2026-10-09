package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// The repository of the files the nodes send.

// receiving is a file being received.
type receiving struct {
	kind   Kind
	mime   MIMEType
	size   int64
	sum    []byte
	nodeID string
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func uuidBytes(u shared.UUID) []byte {
	if u.IsZero() {
		return nil
	}

	return u.Bytes()
}

func nullTime(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: !t.IsZero()}
}

// exists reports whether a file with id exists, in any state.
func (r *Files) exists(ctx context.Context, id shared.UUID) (bool, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).FileExists(ctx, id.Bytes())
	if err != nil {
		return false, fmt.Errorf("file %s exists: %w", id, err)
	}

	return n > 0, nil
}

// addReceiving stores an announced file, receiving, without content.
func (r *Files) addReceiving(ctx context.Context, in Incoming, metadata string, now time.Time) error {
	err := sqlc.New(r.db.Writer(ctx)).InsertReceivingFile(ctx, sqlc.InsertReceivingFileParams{
		ID: in.ID.Bytes(), Kind: string(in.Kind), Name: in.Name(), MimeType: string(in.MIME), SizeBytes: in.Size,
		Sha256: in.SHA256, NodeID: nullString(in.Node), DeviceID: nullString(in.DeviceID.String()),
		PresetID: uuidBytes(in.PresetID), DecoderSessionID: uuidBytes(in.DecoderSessionID), Mode: nullString(in.Mode),
		FrequencyHz: sql.NullInt64{Int64: in.FrequencyHz, Valid: true}, ReceivedStartUtc: nullTime(in.ReceivedStart),
		ReceivedEndUtc: nullTime(in.ReceivedEnd), Metadata: metadata, CreatedAt: now.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("insert file %s: %w", in.ID, err)
	}

	return nil
}

// receiving returns a file being received, or ErrFileNotFound.
func (r *Files) receiving(ctx context.Context, id shared.UUID) (receiving, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).ReceivingFile(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return receiving{}, ErrFileNotFound
	}

	if err != nil {
		return receiving{}, fmt.Errorf("read file %s: %w", id, err)
	}

	return receiving{kind: Kind(row.Kind), mime: MIMEType(row.MimeType), size: row.SizeBytes, sum: row.Sha256, nodeID: row.NodeID.String}, nil
}

// received returns the content bytes stored for a file.
func (r *Files) received(ctx context.Context, id shared.UUID) (int64, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).FileReceivedBytes(ctx, id.Bytes())
	if err != nil {
		return 0, fmt.Errorf("received bytes of file %s: %w", id, err)
	}

	return n, nil
}

// addChunk stores the content of one wire chunk as the next chunk of a
// file being received, and records the activity (a file without content
// for IncompleteAfter is purged). The content is re-chunked in rows of up
// to ChunkSize when the file completes.
func (r *Files) addChunk(ctx context.Context, id shared.UUID, data []byte, now time.Time) error {
	q := sqlc.New(r.db.Writer(ctx))

	next, err := q.NextFileChunk(ctx, id.Bytes())
	if err != nil {
		return fmt.Errorf("next chunk of file %s: %w", id, err)
	}

	if err := q.InsertFileChunk(ctx, sqlc.InsertFileChunkParams{FileID: id.Bytes(), ChunkNo: next, Data: data}); err != nil {
		return fmt.Errorf("insert chunk %d of file %s: %w", next, id, err)
	}

	if err := q.TouchReceivingFile(ctx, sqlc.TouchReceivingFileParams{ReceivedAt: nullTime(now), ID: id.Bytes()}); err != nil {
		return fmt.Errorf("touch file %s: %w", id, err)
	}

	return nil
}

// writeContent stores content as the chunks of a file without any, in
// rows of at most ChunkSize.
func (r *Files) writeContent(ctx context.Context, id shared.UUID, content []byte) error {
	q := sqlc.New(r.db.Writer(ctx))

	for i := 0; i*ChunkSize < len(content); i++ {
		chunk := content[i*ChunkSize : min((i+1)*ChunkSize, len(content))]

		if err := q.InsertFileChunk(ctx, sqlc.InsertFileChunkParams{FileID: id.Bytes(), ChunkNo: int64(i), Data: chunk}); err != nil {
			return fmt.Errorf("insert chunk %d of file %s: %w", i, id, err)
		}
	}

	return nil
}

// receivingBytes returns the announced size of the files a node is
// sending.
func (r *Files) receivingBytes(ctx context.Context, node string) (int64, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).ReceivingBytesOfNode(ctx, nullString(node))
	if err != nil {
		return 0, fmt.Errorf("files received from %s: %w", node, err)
	}

	return n, nil
}

// WriteContent writes the content of a complete file to w, one stored
// chunk at a time.
func (r *Files) WriteContent(ctx context.Context, e Entry, w io.Writer) error {
	q := sqlc.New(r.db.Reader(ctx))

	nums, err := q.FileChunkNumbers(ctx, e.ID.Bytes())
	if err != nil {
		return fmt.Errorf("chunks of file %s: %w", e.ID, err)
	}

	for _, n := range nums {
		data, err := q.FileChunk(ctx, sqlc.FileChunkParams{FileID: e.ID.Bytes(), ChunkNo: n})
		if err != nil {
			return fmt.Errorf("read chunk %d of file %s: %w", n, e.ID, err)
		}

		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write file %s: %w", e.ID, err)
		}
	}

	return nil
}

// FirstChunk returns the first stored chunk of a file (at most ChunkSize
// bytes): the start of a text, for its preview.
func (r *Files) FirstChunk(ctx context.Context, e Entry) ([]byte, error) {
	data, err := sqlc.New(r.db.Reader(ctx)).FileChunk(ctx, sqlc.FileChunkParams{FileID: e.ID.Bytes(), ChunkNo: 0})
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", e.ID, err)
	}

	return data, nil
}

// contentOf returns the stored content of a file, whatever its state.
func (r *Files) contentOf(ctx context.Context, id shared.UUID) ([]byte, error) {
	chunks, err := sqlc.New(r.db.Reader(ctx)).FileChunks(ctx, id.Bytes())
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", id, err)
	}

	return bytes.Join(chunks, nil), nil
}

// completed describes the final content of a received file.
type completed struct {
	content       []byte
	width, height int
	thumbnail     []byte
}

// complete replaces the content of a received file with c and marks it
// complete. It reports false when the file is no longer receiving.
func (r *Files) complete(ctx context.Context, id shared.UUID, c completed) (bool, error) {
	q := sqlc.New(r.db.Writer(ctx))

	if err := q.DeleteFileChunks(ctx, id.Bytes()); err != nil {
		return false, fmt.Errorf("replace content of file %s: %w", id, err)
	}

	if err := r.writeContent(ctx, id, c.content); err != nil {
		return false, err
	}

	sum := sha256Sum(c.content)

	n, err := q.CompleteFile(ctx, sqlc.CompleteFileParams{
		SizeBytes: int64(len(c.content)), Sha256: sum, Width: nullInt(c.width), Height: nullInt(c.height),
		Thumbnail: c.thumbnail, ID: id.Bytes(),
	})
	if err != nil {
		return false, fmt.Errorf("complete file %s: %w", id, err)
	}

	return n == 1, nil
}

// remove deletes a file with its content. It reports whether it existed.
func (r *Files) remove(ctx context.Context, id shared.UUID) (bool, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteFile(ctx, id.Bytes())
	if err != nil {
		return false, fmt.Errorf("delete file %s: %w", id, err)
	}

	return n > 0, nil
}

// entryFields are the columns of a produced file row.
type entryFields struct {
	ID               []byte
	Kind             string
	Name             string
	MimeType         string
	SizeBytes        int64
	Sha256           []byte
	NodeID           sql.NullString
	DeviceID         sql.NullString
	PresetID         []byte
	DecoderSessionID []byte
	Mode             sql.NullString
	FrequencyHz      sql.NullInt64
	ReceivedStartUtc sql.NullInt64
	ReceivedEndUtc   sql.NullInt64
	Width            sql.NullInt64
	Height           sql.NullInt64
	Metadata         string
	HasThumbnail     int64
	CreatedAt        int64
}

func (f entryFields) entry() (Entry, error) {
	id, err := shared.UUIDFromBytes(f.ID)
	if err != nil {
		return Entry{}, fmt.Errorf("file id: %w", err)
	}

	e := Entry{
		ID: id, Kind: Kind(f.Kind), Name: f.Name, MIME: MIMEType(f.MimeType), Size: f.SizeBytes, SHA256: f.Sha256,
		NodeID: f.NodeID.String, DeviceID: f.DeviceID.String, Mode: f.Mode.String, FrequencyHz: f.FrequencyHz.Int64,
		Width: int(f.Width.Int64), Height: int(f.Height.Int64), HasThumbnail: f.HasThumbnail == 1,
		CreatedAt: time.UnixMilli(f.CreatedAt).UTC(),
	}

	if f.ReceivedStartUtc.Valid {
		e.ReceivedStart = time.UnixMilli(f.ReceivedStartUtc.Int64).UTC()
	}

	if f.ReceivedEndUtc.Valid {
		e.ReceivedEnd = time.UnixMilli(f.ReceivedEndUtc.Int64).UTC()
	}

	for _, p := range []struct {
		b  []byte
		to *shared.UUID
	}{{f.PresetID, &e.PresetID}, {f.DecoderSessionID, &e.DecoderSessionID}} {
		if p.b != nil {
			if *p.to, err = shared.UUIDFromBytes(p.b); err != nil {
				return Entry{}, fmt.Errorf("file %s: %w", id, err)
			}
		}
	}

	if err := json.Unmarshal([]byte(f.Metadata), &e.Metadata); err != nil {
		return Entry{}, fmt.Errorf("file %s metadata: %w", id, err)
	}

	return e, nil
}

// Entry returns a complete produced file, or ErrFileNotFound.
func (r *Files) Entry(ctx context.Context, id shared.UUID) (Entry, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).ProducedFile(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrFileNotFound
	}

	if err != nil {
		return Entry{}, fmt.Errorf("read file %s: %w", id, err)
	}

	return entryFields(row).entry()
}

// Thumbnail returns the thumbnail of a complete file, or ErrFileNotFound.
func (r *Files) Thumbnail(ctx context.Context, id shared.UUID) ([]byte, error) {
	b, err := sqlc.New(r.db.Reader(ctx)).FileThumbnail(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFileNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("read thumbnail of file %s: %w", id, err)
	}

	return b, nil
}

// filterParams are the SQL parameters of a filter.
type filterParams struct {
	mimeLike, device, mode, from, to, freqMin, freqMax any
}

func paramsOf(f Filter) filterParams {
	var p filterParams

	if f.Media != "" {
		p.mimeLike = string(f.Media) + "/%"
	}

	if f.DeviceID != "" {
		p.device = f.DeviceID
	}

	if f.Mode != "" {
		p.mode = f.Mode
	}

	if !f.From.IsZero() {
		p.from = f.From.UnixMilli()
	}

	if !f.To.IsZero() {
		p.to = f.To.UnixMilli()
	}

	if f.FreqMin > 0 {
		p.freqMin = f.FreqMin
	}

	if f.FreqMax > 0 {
		p.freqMax = f.FreqMax
	}

	return p
}

// List returns the complete produced files matching f that v may see,
// newest reception first: at most limit from offset.
func (r *Files) List(ctx context.Context, f Filter, v Access, offset, limit int) ([]Entry, error) {
	if v.Denied() {
		return nil, nil
	}

	var visible any
	if !v.All {
		b, err := json.Marshal(v.Devices)
		if err != nil {
			return nil, fmt.Errorf("visible devices: %w", err)
		}

		visible = string(b)
	}

	p := paramsOf(f)

	rows, err := sqlc.New(r.db.Reader(ctx)).ListProducedFiles(ctx, sqlc.ListProducedFilesParams{
		MimeLike: p.mimeLike, DeviceID: p.device, Mode: p.mode, FromUtc: p.from, ToUtc: p.to, FreqMin: p.freqMin,
		FreqMax: p.freqMax, VisibleDevices: visible, OffsetRows: int64(offset), LimitRows: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}

	out := make([]Entry, 0, len(rows))

	for _, row := range rows {
		e, err := entryFields(row).entry()
		if err != nil {
			return nil, err
		}

		out = append(out, e)
	}

	return out, nil
}

// deleteMatching deletes the complete produced files matching f.
func (r *Files) deleteMatching(ctx context.Context, f Filter) (int64, error) {
	p := paramsOf(f)

	n, err := sqlc.New(r.db.Writer(ctx)).DeleteProducedFiles(ctx, sqlc.DeleteProducedFilesParams{
		MimeLike: p.mimeLike, DeviceID: p.device, Mode: p.mode, FromUtc: p.from, ToUtc: p.to, FreqMin: p.freqMin, FreqMax: p.freqMax,
	})
	if err != nil {
		return 0, fmt.Errorf("delete files: %w", err)
	}

	return n, nil
}

// FilterValues returns the devices and modes of the produced files, for
// the gallery filters.
func (r *Files) FilterValues(ctx context.Context) (devices, modes []string, err error) {
	q := sqlc.New(r.db.Reader(ctx))

	devs, err := q.ProducedFileDevices(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("file devices: %w", err)
	}

	ms, err := q.ProducedFileModes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("file modes: %w", err)
	}

	for _, d := range devs {
		devices = append(devices, d.String)
	}

	for _, m := range ms {
		modes = append(modes, m.String)
	}

	return devices, modes, nil
}
