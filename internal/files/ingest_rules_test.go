package files_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/files"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestIngestFinalizesPerNode: a node's committed batch completes its own
// files only, never the files of another node whose batch is not
// committed yet.
func TestIngestFinalizesPerNode(t *testing.T) {
	e := newIngest(t, files.RetentionPolicy{Count: 20})
	ids := shared.NewUUIDv7Generator()
	text := []byte("CQ DE F4XYZ\n")

	ofNode := func(node string) files.Incoming {
		in := incoming(files.KindTextLog, files.MIMEText, text)
		in.ID, _ = ids.New(e.now)
		in.Node = node

		return in
	}

	attic, cellar := ofNode("attic"), ofNode("cellar")

	for _, in := range []files.Incoming{attic, cellar} {
		e.tx(func(ctx context.Context) error {
			if err := e.ingest.Begin(ctx, in, nil, e.now); err != nil {
				return err
			}

			if err := e.ingest.Chunk(ctx, in.Node, in.ID, 0, text, e.now); err != nil {
				return err
			}

			return e.ingest.End(ctx, in.Node, in.ID)
		})
	}

	// The cellar's batch commits first: the attic file stays pending.
	e.ingest.Finalize(context.Background(), "cellar")

	if len(e.published) != 1 || e.published[0].ID != cellar.ID {
		t.Fatalf("after the cellar batch: published %v", e.published)
	}

	e.ingest.Finalize(context.Background(), "attic")

	if len(e.published) != 2 || e.published[1].ID != attic.ID {
		t.Errorf("after the attic batch: published %v", e.published)
	}
}

// TestIngestRules covers the hub-side bounds of a node: the files it may
// have in flight, reception times in the future, and a file the retention
// deletes at once is not announced.
func TestIngestRules(t *testing.T) {
	ids := shared.NewUUIDv7Generator()
	text := []byte("CQ DE F4XYZ\n")

	t.Run("files in flight per node", func(t *testing.T) {
		e := newIngest(t, files.RetentionPolicy{Count: 20})

		for range 7 { // 7 × 8 MiB announced: the seventh exceeds 48 MiB
			in := incoming(files.KindTextLog, files.MIMEText, text)
			in.ID, _ = ids.New(e.now)
			in.Size = files.MaxFileSize

			e.tx(func(ctx context.Context) error { return e.ingest.Begin(ctx, in, nil, e.now) })
		}

		if n := e.count("SELECT count(*) FROM files WHERE blob_state = 'receiving'"); n != 6 {
			t.Errorf("files receiving = %d, want 6", n)
		}
	})

	t.Run("reception in the future", func(t *testing.T) {
		e := newIngest(t, files.RetentionPolicy{Count: 20})
		in := incoming(files.KindTextLog, files.MIMEText, text)
		in.ReceivedStart, in.ReceivedEnd = e.now.Add(time.Hour), e.now.Add(2*time.Hour)

		e.send(in, text, 45<<10, nil)

		if len(e.published) != 1 || !e.published[0].ReceivedStart.Equal(e.now.Add(files.ClockSkewLimit)) ||
			!e.published[0].ReceivedEnd.Equal(e.now.Add(files.ClockSkewLimit)) {
			t.Errorf("published = %+v", e.published)
		}
	})

	t.Run("deleted by the retention", func(t *testing.T) {
		e := newIngest(t, files.RetentionPolicy{Count: 20, MaxBytes: 1})

		e.send(incoming(files.KindTextLog, files.MIMEText, text), text, 45<<10, nil)

		if len(e.published) != 0 || e.count("SELECT count(*) FROM files") != 0 {
			t.Errorf("published %v", e.published)
		}
	})

	t.Run("long FAX page", func(t *testing.T) {
		e := newIngest(t, files.RetentionPolicy{Count: 20})

		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1000, 10_000))); err != nil {
			t.Fatal(err)
		}

		content := buf.Bytes()
		in := incoming(files.KindFAX, files.MIMEPNG, content)
		sum := sha256.Sum256(content)
		in.SHA256 = sum[:]

		e.send(in, content, 45<<10, nil)

		if len(e.published) != 1 || e.published[0].Height != 10_000 {
			t.Errorf("published = %v", e.published)
		}
	})
}
