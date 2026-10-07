// Package files is the files module (TECHNICAL_SPEC §7.1 `files`,
// `file_blobs`, ADR 0010). This part has the admin-uploaded receiver images
// (ADM-004): the use case (upload, restore default, read), the SQLite
// repository (metadata in files, content in file_blobs chunks of at most
// 1 MiB), the image re-encoder and the images section of the admin Site
// page. The images themselves are served by GET /api/v1/branding/{slot}
// (internal/http/api). The files epic (FIL) adds decoder and recording files.
package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Image is a decoded and re-encoded image.
type Image struct {
	Data          []byte
	MIME          MIMEType
	Width, Height int
}

// Clock returns the current time.
type Clock func() time.Time

// Audit actions of the receiver images.
const (
	ActionImageUpdate = "receiver_image.update"
	ActionImageRemove = "receiver_image.remove"
)

// Branding manages the receiver images.
type Branding struct {
	repo  *Files
	tx    *db.DB
	proc  *Processor
	ids   *shared.UUIDv7Generator
	audit audit.Appender
	now   Clock
}

// BrandingDeps are the dependencies of Branding.
type BrandingDeps struct {
	Repo      *Files
	Tx        *db.DB
	Processor *Processor
	IDs       *shared.UUIDv7Generator
	Audit     audit.Appender
	Now       Clock
}

// NewBranding returns the use case.
func NewBranding(d BrandingDeps) *Branding {
	return &Branding{repo: d.Repo, tx: d.Tx, proc: d.Processor, ids: d.IDs, audit: d.Audit, now: d.Now}
}

// Upload validates, re-encodes and stores an image for slot, replacing the
// previous one in the same transaction. The caller bounds data to
// slot.MaxUpload() before reading it; a larger upload is refused here too.
func (b *Branding) Upload(ctx context.Context, by shared.UUID, slot Slot, data []byte) (*File, error) {
	if int64(len(data)) > slot.MaxUpload() {
		return nil, ErrImageTooLarge.WithDetail(fmt.Sprintf("the %s must not exceed %d KiB", slot.Name(), slot.MaxUpload()>>10))
	}

	img, err := b.proc.Reencode(ctx, data, slot.Output(), slot.MaxPixels())
	if err != nil {
		return nil, err
	}

	if len(img.Data) > MaxStoredImage {
		return nil, ErrImageTooLarge.WithDetail("the re-encoded image exceeds 2 MiB; upload a smaller image")
	}

	now := b.now()

	id, err := b.ids.New(now)
	if err != nil {
		return nil, fmt.Errorf("file id: %w", err)
	}

	f, err := NewImage(ImageSpec{
		ID: id, Kind: slot.Kind(), MIME: img.MIME, Content: img.Data, Width: img.Width, Height: img.Height,
		UploadedBy: by, At: now,
	})
	if err != nil {
		return nil, err
	}

	err = b.tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := b.repo.DeleteKind(ctx, slot.Kind()); err != nil {
			return err
		}

		if err := b.repo.Add(ctx, f, img.Data); err != nil {
			return err
		}

		return b.audit.Append(ctx, audit.Record{
			Action: ActionImageUpdate, TargetType: "receiver_image", TargetID: slot.Name(),
			After: map[string]string{"file_id": f.ID().String(), "mime_type": string(f.MIME()), "size_bytes": fmt.Sprint(f.Size())},
		})
	})
	if err != nil {
		return nil, err
	}

	return f, nil
}

// Remove restores the default of slot (no image) and reports whether an
// image was removed.
func (b *Branding) Remove(ctx context.Context, slot Slot) (bool, error) {
	removed := false

	err := b.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := b.repo.DeleteKind(ctx, slot.Kind())
		if err != nil || n == 0 {
			return err
		}

		removed = true

		return b.audit.Append(ctx, audit.Record{Action: ActionImageRemove, TargetType: "receiver_image", TargetID: slot.Name()})
	})

	return removed, err
}

// Current returns the image of slot, or ErrImageNotSet.
func (b *Branding) Current(ctx context.Context, slot Slot) (*File, error) {
	f, err := b.repo.LatestOfKind(ctx, slot.Kind())
	if errors.Is(err, ErrFileNotFound) {
		return nil, ErrImageNotSet
	}

	return f, err
}

// Content returns the image of slot with its content.
func (b *Branding) Content(ctx context.Context, slot Slot) (*File, []byte, error) {
	f, err := b.Current(ctx, slot)
	if err != nil {
		return nil, nil, err
	}

	data, err := b.repo.Content(ctx, f)
	if err != nil {
		return nil, nil, err
	}

	return f, data, nil
}
