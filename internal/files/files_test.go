package files_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/files"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestFiles(t *testing.T) {
	ctx := context.Background()
	tx := dbtest.NewSQLite(t)
	repo := files.NewFiles(tx)
	reader := tx

	content := bytes.Repeat([]byte("0123456789abcdef"), (files.ChunkSize*2+100)/16)

	f, err := files.NewImage(files.ImageSpec{
		ID: shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd"), Kind: files.KindReceiverPhoto, MIME: files.MIMEJPEG,
		Content: content, Width: 1, Height: 1, At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := tx.WithinTx(ctx, func(ctx context.Context) error { return repo.Add(ctx, f, content) }); err != nil {
		t.Fatal(err)
	}

	var chunks int
	if err := reader.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM file_blobs").Scan(&chunks); err != nil || chunks != 3 {
		t.Errorf("chunks = %d, %v", chunks, err)
	}

	got, err := repo.LatestOfKind(ctx, files.KindReceiverPhoto)
	if err != nil || got.SHA256() != f.SHA256() {
		t.Fatalf("latest = %v, %v", got, err)
	}

	data, err := repo.Content(ctx, got)
	if err != nil || !bytes.Equal(data, content) {
		t.Errorf("content differs: %v", err)
	}

	if n, err := repo.DeleteKind(ctx, files.KindReceiverPhoto); err != nil || n != 1 {
		t.Errorf("delete = %d, %v", n, err)
	}

	if err := reader.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM file_blobs").Scan(&chunks); err != nil || chunks != 0 {
		t.Errorf("chunks after delete = %d", chunks)
	}
}
