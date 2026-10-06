// Package sqlite implements the files repository for the SQLite dialect:
// metadata in files, content in file_blobs chunks of at most 1 MiB.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/files/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Files is the SQLite files repository.
type Files struct{ db db.Adapter }

var _ domain.Repository = (*Files)(nil)

// NewFiles returns the repository.
func NewFiles(a db.Adapter) *Files { return &Files{db: a} }

func nullInt(v int) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: v > 0} }

// Add implements domain.Repository. The caller runs it in a transaction
// with the deletion of the previous file.
func (r *Files) Add(ctx context.Context, f *domain.File, content []byte) error {
	q := sqlc.New(r.db.Writer(ctx))
	sum := f.SHA256()

	var by []byte
	if !f.UploadedBy().IsZero() {
		by = f.UploadedBy().Bytes()
	}

	err := q.InsertFile(ctx, sqlc.InsertFileParams{
		ID: f.ID().Bytes(), Kind: string(f.Kind()), Name: f.Name(), MimeType: string(f.MIME()), SizeBytes: f.Size(),
		Sha256: sum[:], BlobState: "complete", Visibility: string(f.Visibility()),
		Width: nullInt(f.Width()), Height: nullInt(f.Height()), UploadedBy: by, CreatedAt: f.CreatedAt().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("insert file %s: %w", f.ID(), err)
	}

	for i := 0; i*domain.ChunkSize < len(content); i++ {
		chunk := content[i*domain.ChunkSize : min((i+1)*domain.ChunkSize, len(content))]

		if err := q.InsertFileChunk(ctx, sqlc.InsertFileChunkParams{FileID: f.ID().Bytes(), ChunkNo: int64(i), Data: chunk}); err != nil {
			return fmt.Errorf("insert chunk %d of file %s: %w", i, f.ID(), err)
		}
	}

	return nil
}

// LatestOfKind implements domain.Repository.
func (r *Files) LatestOfKind(ctx context.Context, kind domain.Kind) (*domain.File, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).LatestFileOfKind(ctx, string(kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrFileNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("latest file of kind %s: %w", kind, err)
	}

	id, err := shared.UUIDFromBytes(row.ID)
	if err != nil {
		return nil, fmt.Errorf("file id: %w", err)
	}

	var by shared.UUID
	if row.UploadedBy != nil {
		if by, err = shared.UUIDFromBytes(row.UploadedBy); err != nil {
			return nil, fmt.Errorf("file %s uploader: %w", id, err)
		}
	}

	f, err := domain.RehydrateFile(domain.FileState{
		ID: id, Kind: domain.Kind(row.Kind), Name: row.Name, MIME: domain.MIMEType(row.MimeType), Size: row.SizeBytes,
		SHA256: row.Sha256, Visibility: domain.Visibility(row.Visibility), Width: int(row.Width.Int64), Height: int(row.Height.Int64),
		UploadedBy: by, CreatedAt: time.UnixMilli(row.CreatedAt).UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("file %s: %w", id, err)
	}

	return f, nil
}

// Content implements domain.Repository: the chunks in order.
func (r *Files) Content(ctx context.Context, f *domain.File) ([]byte, error) {
	chunks, err := sqlc.New(r.db.Reader(ctx)).FileChunks(ctx, f.ID().Bytes())
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", f.ID(), err)
	}

	data := bytes.Join(chunks, nil)
	if int64(len(data)) != f.Size() {
		return nil, fmt.Errorf("read file %s: %d bytes, want %d", f.ID(), len(data), f.Size())
	}

	return data, nil
}

// DeleteKind implements domain.Repository.
func (r *Files) DeleteKind(ctx context.Context, kind domain.Kind) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteFilesOfKind(ctx, string(kind))
	if err != nil {
		return 0, fmt.Errorf("delete files of kind %s: %w", kind, err)
	}

	return int(n), nil
}
