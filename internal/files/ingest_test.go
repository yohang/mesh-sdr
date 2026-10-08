package files_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/files"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var received = time.Date(2026, 10, 6, 14, 32, 5, 0, time.UTC)

// ingestEnv is an ingest over a migrated database, with the files it
// announced.
type ingestEnv struct {
	t         *testing.T
	db        *db.DB
	repo      *files.Files
	ingest    *files.Ingest
	published []files.Entry
	now       time.Time
}

func newIngest(t *testing.T, policy files.RetentionPolicy) *ingestEnv {
	t.Helper()

	e := &ingestEnv{t: t, db: dbtest.NewSQLite(t), now: received.Add(3 * time.Minute)}
	e.repo = files.NewFiles(e.db)
	e.ingest = files.NewIngest(files.IngestDeps{
		Repo: e.repo, Tx: e.db, Processor: files.NewProcessor(),
		Retention: files.NewRetention(e.db, func() files.RetentionPolicy { return policy }, func() time.Time { return e.now }),
		Published: func(_ context.Context, f files.Entry) { e.published = append(e.published, f) },
		Logger:    slog.New(slog.DiscardHandler),
	})

	return e
}

// tx runs fn like the control channel runs a batch of node events.
func (e *ingestEnv) tx(fn func(ctx context.Context) error) {
	e.t.Helper()

	if err := e.db.WithinTx(context.Background(), fn); err != nil {
		e.t.Fatal(err)
	}
}

func incoming(kind files.Kind, mime files.MIMEType, content []byte) files.Incoming {
	sum := sha256.Sum256(content)

	return files.Incoming{
		ID: shared.MustParseUUID("0199a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b"), Node: "attic", Kind: kind, MIME: mime,
		Size: int64(len(content)), SHA256: sum[:], DeviceID: shared.MustDeviceID("hf"), Mode: string(kind),
		FrequencyHz: 14_230_000, ReceivedStart: received, ReceivedEnd: received.Add(110 * time.Second),
		Metadata: json.RawMessage(`{"sstv_mode":"Scottie 1","vis_code":60}`),
	}
}

// send runs the events of a whole file in one batch, in chunks of size,
// then finalizes it.
func (e *ingestEnv) send(in files.Incoming, content []byte, size int, offset *int64) {
	e.t.Helper()

	e.tx(func(ctx context.Context) error {
		if err := e.ingest.Begin(ctx, in, offset, e.now); err != nil {
			return err
		}

		for off := 0; off < len(content); off += size {
			if err := e.ingest.Chunk(ctx, in.Node, in.ID, int64(off), content[off:min(off+size, len(content))], e.now); err != nil {
				return err
			}
		}

		return e.ingest.End(ctx, in.Node, in.ID)
	})

	e.ingest.Finalize(context.Background(), "attic")
}

func (e *ingestEnv) count(query string) int {
	e.t.Helper()

	var n int
	if err := e.db.Reader(context.Background()).QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}

	return n
}

// photo is a PNG with a text chunk (metadata the re-encoding drops).
func photo(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{R: uint8(x), G: 100, B: 200, A: 255})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	// A tEXt chunk after the header: "Comment" = "GPS 50.63".
	body := []byte("tEXtComment\x00GPS 50.63")
	text := binary.BigEndian.AppendUint32(nil, uint32(len(body)-4))
	text = append(text, body...)
	text = binary.BigEndian.AppendUint32(text, crc32.ChecksumIEEE(body))

	return append(append(append([]byte{}, data[:33]...), text...), data[33:]...)
}

// TestIngestImage covers FIL-005 and FIL-008 for an SSTV image: stored with
// its reception metadata, re-encoded without metadata, with a thumbnail,
// complete and announced.
func TestIngestImage(t *testing.T) {
	e := newIngest(t, files.RetentionPolicy{Count: 20})
	content := photo(t, 640, 496)
	in := incoming(files.KindSSTV, files.MIMEPNG, content)
	skew := int64(7_000)

	e.send(in, content, 45<<10, &skew)

	if len(e.published) != 1 {
		t.Fatalf("published = %v", e.published)
	}

	f := e.published[0]
	if f.ID != in.ID || f.Name != "SSTV-261006-143205-14230.png" || f.Kind != files.KindSSTV || f.NodeID != "attic" ||
		f.DeviceID != "hf" || f.FrequencyHz != 14_230_000 || !f.ReceivedStart.Equal(received) ||
		!f.ReceivedEnd.Equal(received.Add(110*time.Second)) || f.Width != 640 || f.Height != 496 || !f.HasThumbnail {
		t.Errorf("entry = %+v", f)
	}

	if f.Metadata["sstv_mode"] != "Scottie 1" || f.Metadata["clock_skew_ms"] != float64(7000) {
		t.Errorf("metadata = %v", f.Metadata)
	}

	data, err := e.repo.EntryContent(context.Background(), f)
	if err != nil || bytes.Contains(data, []byte("GPS 50.63")) || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Errorf("stored content: %v (metadata must be dropped)", err)
	}

	thumb, err := e.repo.Thumbnail(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb))
	if err != nil || format != "jpeg" || cfg.Width != files.ThumbnailSide || cfg.Height != 248 {
		t.Errorf("thumbnail = %s %dx%d, %v", format, cfg.Width, cfg.Height, err)
	}
}

// TestIngestRechunks: the content of the wire chunks is stored in chunks
// of at most 1 MiB, whatever the batches.
func TestIngestRechunks(t *testing.T) {
	e := newIngest(t, files.RetentionPolicy{Count: 20})
	content := bytes.Repeat([]byte("CQ CQ DE F4XYZ\n"), 200_000) // 3 000 000 bytes
	in := incoming(files.KindTextLog, files.MIMEText, content)
	in.Metadata = nil

	// One batch per wire chunk, like a slow node.
	e.tx(func(ctx context.Context) error { return e.ingest.Begin(ctx, in, nil, e.now) })

	for off := 0; off < len(content); off += 45 << 10 {
		e.tx(func(ctx context.Context) error {
			return e.ingest.Chunk(ctx, "attic", in.ID, int64(off), content[off:min(off+45<<10, len(content))], e.now)
		})
	}

	// While the file is received, each wire chunk is one row (no rewrite).
	if n := e.count("SELECT count(*) FROM file_blobs"); n != 66 {
		t.Errorf("chunks while receiving = %d, want 66", n)
	}

	e.tx(func(ctx context.Context) error { return e.ingest.End(ctx, "attic", in.ID) })
	e.ingest.Finalize(context.Background(), "attic")

	if len(e.published) != 1 || e.published[0].Name != "LOG-261006-143205-14230.txt" || e.published[0].Metadata["clock_skew_ms"] != nil {
		t.Fatalf("published = %+v", e.published)
	}

	if n := e.count("SELECT count(*) FROM file_blobs WHERE length(data) > 1048576"); n != 0 {
		t.Errorf("%d chunks over 1 MiB", n)
	}

	if n := e.count("SELECT count(*) FROM file_blobs"); n != 3 {
		t.Errorf("chunks = %d, want 3", n)
	}

	var buf bytes.Buffer
	if err := e.repo.WriteContent(context.Background(), e.published[0], &buf); err != nil || !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("content differs: %v", err)
	}
}

// TestIngestRefuses: a file breaking a rule is deleted, never an error (the
// node's events are acknowledged anyway).
func TestIngestRefuses(t *testing.T) {
	img := photo(t, 32, 32)
	text := []byte("CQ DE F4XYZ\n")

	with := func(kind files.Kind, mime files.MIMEType, content []byte, change func(in *files.Incoming)) func() (files.Incoming, []byte) {
		return func() (files.Incoming, []byte) {
			in := incoming(kind, mime, content)
			if change != nil {
				change(&in)
			}

			return in, content
		}
	}

	tests := []struct {
		name string
		file func() (files.Incoming, []byte)
		send func(e *ingestEnv, in files.Incoming, content []byte)
	}{
		{"kind a node cannot send", with(files.KindTextLog, files.MIMEText, text, func(in *files.Incoming) { in.Kind = "receiver_avatar" }), nil},
		{"wrong media type", with(files.KindTextLog, files.MIMEPNG, text, nil), nil},
		{"over the cap", with(files.KindSSTV, files.MIMEPNG, img, func(in *files.Incoming) { in.Size = files.MaxFileSize + 1 }), nil},
		{"no frequency", with(files.KindSSTV, files.MIMEPNG, img, func(in *files.Incoming) { in.FrequencyHz = 0 }), nil},
		{"metadata not an object", with(files.KindSSTV, files.MIMEPNG, img, func(in *files.Incoming) { in.Metadata = json.RawMessage(`[1]`) }), nil},
		{"digest mismatch", with(files.KindSSTV, files.MIMEPNG, img, func(in *files.Incoming) { in.SHA256 = make([]byte, 32) }), nil},
		{"text that is not UTF-8", with(files.KindTextLog, files.MIMEText, []byte("\xff\xfe"), nil), nil},
		{"image that is not a PNG", with(files.KindSSTV, files.MIMEPNG, []byte("<svg/>"), nil), nil},
		{"truncated PNG", with(files.KindSSTV, files.MIMEPNG, img[:100], nil), nil},
		{"chunk out of order", with(files.KindSSTV, files.MIMEPNG, img, nil), func(e *ingestEnv, in files.Incoming, content []byte) {
			e.tx(func(ctx context.Context) error {
				if err := e.ingest.Begin(ctx, in, nil, e.now); err != nil {
					return err
				}

				return e.ingest.Chunk(ctx, "attic", in.ID, 10, content[10:], e.now)
			})
		}},
		{"another node's file", with(files.KindSSTV, files.MIMEPNG, img, nil), func(e *ingestEnv, in files.Incoming, content []byte) {
			e.tx(func(ctx context.Context) error {
				if err := e.ingest.Begin(ctx, in, nil, e.now); err != nil {
					return err
				}

				if err := e.ingest.Chunk(ctx, "cellar", in.ID, 0, content, e.now); err != nil {
					return err
				}

				return e.ingest.End(ctx, "cellar", in.ID)
			})
			e.ingest.Finalize(context.Background(), "attic")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newIngest(t, files.RetentionPolicy{Count: 20})
			in, content := tt.file()

			if tt.send != nil {
				tt.send(e, in, content)
			} else {
				e.send(in, content, 45<<10, nil)
			}

			if len(e.published) != 0 {
				t.Errorf("published %v", e.published)
			}

			if n := e.count("SELECT count(*) FROM files WHERE blob_state = 'complete'"); n != 0 {
				t.Errorf("%d complete files", n)
			}

			if n := e.count("SELECT count(*) FROM file_blobs"); n != 0 {
				t.Errorf("%d chunks stored", n)
			}
		})
	}
}

// TestRetention covers FIL-004: the newest files per kind, the age and the
// total size, oldest first; files incomplete for 10 minutes go; the
// receiver images are kept.
func TestRetention(t *testing.T) {
	ctx := context.Background()
	policy := files.RetentionPolicy{Count: 2}
	e := newIngest(t, files.RetentionPolicy{})
	r := files.NewRetention(e.db, func() files.RetentionPolicy { return policy }, func() time.Time { return e.now })
	ids := shared.NewUUIDv7Generator()

	// Five text files, one minute apart.
	for i := range 5 {
		content := bytes.Repeat([]byte{'a' + byte(i)}, 1000)
		in := incoming(files.KindTextLog, files.MIMEText, content)
		in.ID, _ = ids.New(e.now)
		e.send(in, content, 45<<10, nil)
		e.now = e.now.Add(time.Minute)
	}

	// An SSTV image, and a receiver image.
	img := photo(t, 16, 16)
	in := incoming(files.KindSSTV, files.MIMEPNG, img)
	in.ID, _ = ids.New(e.now)
	e.send(in, img, 45<<10, nil)

	b := files.NewBranding(files.BrandingDeps{
		Repo: e.repo, Tx: e.db, Processor: files.NewProcessor(), IDs: ids, Audit: &audit.Records{}, Now: func() time.Time { return received },
	})
	if _, err := b.Upload(ctx, shared.UUID{}, files.SlotAvatar, grayPNG(t, 8, 8)); err != nil {
		t.Fatal(err)
	}

	if n, err := r.Run(ctx); err != nil || n != 3 {
		t.Errorf("count: deleted %d, %v; want 3", n, err)
	}

	if n := e.count("SELECT count(*) FROM files WHERE kind = 'text_log'"); n != 2 {
		t.Errorf("text files kept = %d", n)
	}

	// The two text files kept are the newest.
	if n := e.count("SELECT count(*) FROM files WHERE kind = 'text_log' AND hex(substr((SELECT data FROM file_blobs WHERE file_id = files.id), 1, 1)) IN ('64', '65')"); n != 2 {
		t.Errorf("newest text files kept = %d", n)
	}

	// Age: the files stored more than 30 s ago go (the text files, not the
	// image stored last).
	policy = files.RetentionPolicy{Count: 20, MaxAge: 30 * time.Second}
	if n, err := r.Run(ctx); err != nil || n != 2 {
		t.Errorf("age: deleted %d, %v; want 2", n, err)
	}

	// Size: under a cap of one byte, every produced file goes.
	policy = files.RetentionPolicy{Count: 20, MaxBytes: 1}
	if n, err := r.Run(ctx); err != nil || n != 1 {
		t.Errorf("size: deleted %d, %v; want 1", n, err)
	}

	if n := e.count("SELECT count(*) FROM files WHERE kind = 'receiver_avatar'"); n != 1 {
		t.Errorf("receiver images = %d", n)
	}

	// A file left receiving is deleted with its chunks after 10 minutes.
	late := incoming(files.KindTextLog, files.MIMEText, []byte("abc"))
	late.ID, _ = ids.New(e.now)
	e.tx(func(ctx context.Context) error {
		if err := e.ingest.Begin(ctx, late, nil, e.now); err != nil {
			return err
		}

		return e.ingest.Chunk(ctx, "attic", late.ID, 0, []byte("ab"), e.now)
	})

	if n, _ := r.Run(ctx); n != 0 {
		t.Errorf("a recent incomplete file was purged")
	}

	// Content still arriving keeps it: the delay runs from the last chunk.
	e.now = e.now.Add(files.IncompleteAfter - time.Minute)
	e.tx(func(ctx context.Context) error { return e.ingest.Chunk(ctx, "attic", late.ID, 2, []byte("c"), e.now) })
	e.now = e.now.Add(2 * time.Minute)

	if n, _ := r.Run(ctx); n != 0 {
		t.Errorf("an incomplete file still receiving was purged")
	}

	e.now = e.now.Add(files.IncompleteAfter)

	if n, err := r.Run(ctx); err != nil || n != 1 || e.count("SELECT count(*) FROM file_blobs WHERE file_id NOT IN (SELECT id FROM files)") != 0 {
		t.Errorf("incomplete: deleted %d, %v", n, err)
	}

	if rows, size, sized, err := r.Stats(ctx); err != nil || rows != 0 || size != 0 || !sized {
		t.Errorf("stats = %d %d %v %v", rows, size, sized, err)
	}

	if got := (files.RetentionPolicy{Count: 20, MaxAge: 30 * 24 * time.Hour, MaxBytes: 2 << 30}).String(); got != "20 per kind, 30d, 2.0 GiB" {
		t.Errorf("policy = %q", got)
	}
}
