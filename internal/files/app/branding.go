// Package app holds the files use cases. This part has the receiver images
// (ADM-004, ADR 0010): upload, restore default, read.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/yohang/mesh-sdr/internal/files/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Image is a decoded and re-encoded image.
type Image struct {
	Data          []byte
	MIME          domain.MIMEType
	Width, Height int
}

// ImageProcessor validates an upload (magic bytes, dimensions, colour
// model, at most maxPixels pixels, read before decoding) and re-encodes it
// to out, dropping its metadata.
type ImageProcessor interface {
	Reencode(ctx context.Context, data []byte, out domain.MIMEType, maxPixels int) (Image, error)
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// IDGenerator returns new UUIDv7 ids.
type IDGenerator interface {
	New(now time.Time) (shared.UUID, error)
}

// Clock returns the current time.
type Clock func() time.Time

// Actor is who changes the images.
type Actor struct {
	User      shared.UUID
	IP        netip.Addr
	RequestID string
}

// Audit actions of the receiver images.
const (
	ActionImageUpdate = "receiver_image.update"
	ActionImageRemove = "receiver_image.remove"
)

// AuditRecord is one audited change of a receiver image.
type AuditRecord struct {
	Actor  Actor
	At     time.Time
	Action string
	Slot   string
	After  map[string]string
}

// Auditor appends audit records in the caller's transaction.
type Auditor interface {
	RecordImage(ctx context.Context, r AuditRecord) error
}

// Branding manages the receiver images.
type Branding struct {
	repo  domain.Repository
	tx    Transactor
	proc  ImageProcessor
	ids   IDGenerator
	audit Auditor
	now   Clock
}

// BrandingDeps are the dependencies of Branding.
type BrandingDeps struct {
	Repo      domain.Repository
	Tx        Transactor
	Processor ImageProcessor
	IDs       IDGenerator
	Audit     Auditor
	Now       Clock
}

// NewBranding returns the use case.
func NewBranding(d BrandingDeps) *Branding {
	return &Branding{repo: d.Repo, tx: d.Tx, proc: d.Processor, ids: d.IDs, audit: d.Audit, now: d.Now}
}

// Upload validates, re-encodes and stores an image for slot, replacing the
// previous one in the same transaction. The caller bounds data to
// slot.MaxUpload() before reading it; a larger upload is refused here too.
func (b *Branding) Upload(ctx context.Context, actor Actor, slot domain.Slot, data []byte) (*domain.File, error) {
	if int64(len(data)) > slot.MaxUpload() {
		return nil, domain.ErrImageTooLarge.WithDetail(fmt.Sprintf("the %s must not exceed %d KiB", slot.Name(), slot.MaxUpload()>>10))
	}

	img, err := b.proc.Reencode(ctx, data, slot.Output(), slot.MaxPixels())
	if err != nil {
		return nil, err
	}

	if len(img.Data) > domain.MaxStoredImage {
		return nil, domain.ErrImageTooLarge.WithDetail("the re-encoded image exceeds 2 MiB; upload a smaller image")
	}

	now := b.now()

	id, err := b.ids.New(now)
	if err != nil {
		return nil, fmt.Errorf("file id: %w", err)
	}

	f, err := domain.NewImage(domain.ImageSpec{
		ID: id, Kind: slot.Kind(), MIME: img.MIME, Content: img.Data, Width: img.Width, Height: img.Height,
		UploadedBy: actor.User, At: now,
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

		return b.audit.RecordImage(ctx, AuditRecord{
			Actor: actor, At: now, Action: ActionImageUpdate, Slot: slot.Name(),
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
func (b *Branding) Remove(ctx context.Context, actor Actor, slot domain.Slot) (bool, error) {
	removed := false

	err := b.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := b.repo.DeleteKind(ctx, slot.Kind())
		if err != nil || n == 0 {
			return err
		}

		removed = true

		return b.audit.RecordImage(ctx, AuditRecord{Actor: actor, At: b.now(), Action: ActionImageRemove, Slot: slot.Name()})
	})

	return removed, err
}

// Current returns the image of slot, or domain.ErrImageNotSet.
func (b *Branding) Current(ctx context.Context, slot domain.Slot) (*domain.File, error) {
	f, err := b.repo.LatestOfKind(ctx, slot.Kind())
	if errors.Is(err, domain.ErrFileNotFound) {
		return nil, domain.ErrImageNotSet
	}

	return f, err
}

// Content returns the image of slot with its content.
func (b *Branding) Content(ctx context.Context, slot domain.Slot) (*domain.File, []byte, error) {
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
