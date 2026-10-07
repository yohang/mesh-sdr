// Package repotest holds the contract suite of the files repository (ADR
// 0006): every dialect adapter runs it against a migrated database.
package repotest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/files/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Factory opens a fresh repository on a migrated database, with its
// adapter (transactions and row counts).
type Factory func(t *testing.T) (domain.Repository, *db.DB)

// Run runs the repository contract: content larger than a chunk is stored
// in several chunks and read back in order.
func Run(t *testing.T, open Factory) {
	ctx := context.Background()
	repo, tx := open(t)
	reader := tx

	content := bytes.Repeat([]byte("0123456789abcdef"), (domain.ChunkSize*2+100)/16)

	f, err := domain.NewImage(domain.ImageSpec{
		ID: shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd"), Kind: domain.KindReceiverPhoto, MIME: domain.MIMEJPEG,
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

	got, err := repo.LatestOfKind(ctx, domain.KindReceiverPhoto)
	if err != nil || got.SHA256() != f.SHA256() {
		t.Fatalf("latest = %v, %v", got, err)
	}

	data, err := repo.Content(ctx, got)
	if err != nil || !bytes.Equal(data, content) {
		t.Errorf("content differs: %v", err)
	}

	if n, err := repo.DeleteKind(ctx, domain.KindReceiverPhoto); err != nil || n != 1 {
		t.Errorf("delete = %d, %v", n, err)
	}

	if err := reader.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM file_blobs").Scan(&chunks); err != nil || chunks != 0 {
		t.Errorf("chunks after delete = %d", chunks)
	}
}
