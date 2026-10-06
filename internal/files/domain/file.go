// Package domain is the files domain (TECHNICAL_SPEC §7.1 `files`,
// `file_blobs`, ADR 0010). This part models the admin-uploaded receiver
// images; the files epic (FIL) adds decoder and recording files.
package domain

import (
	"context"
	"crypto/sha256"
	"strconv"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Errors of the files domain.
var (
	ErrInvalidFile       = shared.NewError(shared.KindInvalid, "invalid_file", "invalid file")
	ErrFileNotFound      = shared.NewError(shared.KindNotFound, "file_not_found", "file not found")
	ErrUnsupportedImage  = shared.NewError(shared.KindInvalid, "unsupported_image", "the image must be a PNG, JPEG or WebP file")
	ErrImageTooLarge     = shared.NewError(shared.KindInvalid, "image_too_large", "the image is too large")
	ErrImageDimensions   = shared.NewError(shared.KindInvalid, "image_dimensions", "the image is larger than 8192 × 8192 pixels")
	ErrUnknownImageSlot  = shared.NewError(shared.KindNotFound, "unknown_image_slot", "unknown receiver image")
	ErrImageNotSet       = shared.NewError(shared.KindNotFound, "image_not_set", "no image is set")
	ErrUnsupportedKind   = shared.NewError(shared.KindInvalid, "unsupported_file_kind", "this kind of file is not supported yet")
	ErrInvalidMIMEType   = shared.NewError(shared.KindInvalid, "invalid_mime_type", "file type not allowed")
	ErrInvalidVisibility = shared.NewError(shared.KindInvalid, "invalid_visibility", "invalid file visibility")
)

// Kind is the kind of a file (`files.kind`).
type Kind string

// Kinds. Only the receiver kinds are produced in this part.
const (
	KindReceiverAvatar Kind = "receiver_avatar"
	KindReceiverPhoto  Kind = "receiver_photo"
)

// MIMEType is an allowed media type (`files.mime_type` allow-list).
type MIMEType string

// Image media types.
const (
	MIMEPNG  MIMEType = "image/png"
	MIMEJPEG MIMEType = "image/jpeg"
	MIMEWebP MIMEType = "image/webp"
)

// Extension returns the file name extension of the type.
func (m MIMEType) Extension() string {
	switch m {
	case MIMEPNG:
		return "png"
	case MIMEJPEG:
		return "jpg"
	case MIMEWebP:
		return "webp"
	default:
		return "bin"
	}
}

// Visibility tells who may download a file.
type Visibility string

// Visibilities.
const (
	VisibilityPublic     Visibility = "public"
	VisibilityRegistered Visibility = "registered"
	VisibilityAdmin      Visibility = "admin"
)

// ChunkSize is the size of a content chunk (`file_blobs.data`, 1 MiB).
const ChunkSize = 1 << 20

// File is the metadata of a stored file. Its content is stored in chunks
// with it; a File always describes complete content.
type File struct {
	id         shared.UUID
	kind       Kind
	name       string
	mime       MIMEType
	size       int64
	sum        [sha256.Size]byte
	visibility Visibility
	width      int
	height     int
	uploadedBy shared.UUID
	createdAt  time.Time
}

// ImageSpec describes an uploaded image.
type ImageSpec struct {
	ID         shared.UUID
	Kind       Kind
	MIME       MIMEType
	Content    []byte
	Width      int
	Height     int
	UploadedBy shared.UUID // zero when unknown
	At         time.Time
}

// NewImage returns the file of an admin-uploaded receiver image. The name is
// generated (`<prefix>-<yymmdd>-<HHMMSS>.<ext>`), never taken from the
// upload.
func NewImage(s ImageSpec) (*File, error) {
	switch {
	case s.Kind != KindReceiverAvatar && s.Kind != KindReceiverPhoto:
		return nil, ErrUnsupportedKind.WithDetail("unsupported file kind " + strconv.Quote(string(s.Kind)))
	case s.MIME != MIMEPNG && s.MIME != MIMEJPEG && s.MIME != MIMEWebP:
		return nil, ErrInvalidMIMEType
	case s.ID.IsZero(), len(s.Content) == 0, s.Width < 1, s.Height < 1, s.At.IsZero():
		return nil, ErrInvalidFile
	}

	at := s.At.UTC().Truncate(time.Millisecond)
	prefix := "avatar"

	if s.Kind == KindReceiverPhoto {
		prefix = "panorama"
	}

	return &File{
		id: s.ID, kind: s.Kind, mime: s.MIME, size: int64(len(s.Content)), sum: sha256.Sum256(s.Content),
		name:       prefix + "-" + at.Format("060102-150405") + "." + s.MIME.Extension(),
		visibility: VisibilityPublic, width: s.Width, height: s.Height, uploadedBy: s.UploadedBy, createdAt: at,
	}, nil
}

// FileState holds the persisted fields of a file (rehydration).
type FileState struct {
	ID         shared.UUID
	Kind       Kind
	Name       string
	MIME       MIMEType
	Size       int64
	SHA256     []byte
	Visibility Visibility
	Width      int
	Height     int
	UploadedBy shared.UUID
	CreatedAt  time.Time
}

// RehydrateFile rebuilds a file from its persisted state.
func RehydrateFile(s FileState) (*File, error) {
	if s.ID.IsZero() || s.Name == "" || len(s.SHA256) != sha256.Size || s.Size < 0 {
		return nil, ErrInvalidFile
	}

	switch s.Visibility {
	case VisibilityPublic, VisibilityRegistered, VisibilityAdmin:
	default:
		return nil, ErrInvalidVisibility
	}

	f := &File{
		id: s.ID, kind: s.Kind, name: s.Name, mime: s.MIME, size: s.Size, visibility: s.Visibility,
		width: s.Width, height: s.Height, uploadedBy: s.UploadedBy, createdAt: s.CreatedAt,
	}
	copy(f.sum[:], s.SHA256)

	return f, nil
}

// ID returns the file id.
func (f *File) ID() shared.UUID { return f.id }

// Kind returns the kind.
func (f *File) Kind() Kind { return f.kind }

// Name returns the generated display and download name.
func (f *File) Name() string { return f.name }

// MIME returns the media type.
func (f *File) MIME() MIMEType { return f.mime }

// Size returns the content size in bytes.
func (f *File) Size() int64 { return f.size }

// SHA256 returns the content digest.
func (f *File) SHA256() [sha256.Size]byte { return f.sum }

// Visibility returns who may download the file.
func (f *File) Visibility() Visibility { return f.visibility }

// Width returns the image width in pixels.
func (f *File) Width() int { return f.width }

// Height returns the image height in pixels.
func (f *File) Height() int { return f.height }

// UploadedBy returns the uploader (zero when unknown).
func (f *File) UploadedBy() shared.UUID { return f.uploadedBy }

// CreatedAt returns when the file was stored.
func (f *File) CreatedAt() time.Time { return f.createdAt }

// Repository stores files with their content. Writes join the caller's
// transaction.
type Repository interface {
	// Add stores f and its content, in chunks of ChunkSize.
	Add(ctx context.Context, f *File, content []byte) error
	// LatestOfKind returns the newest file of kind, or ErrFileNotFound.
	LatestOfKind(ctx context.Context, kind Kind) (*File, error)
	// Content returns the content of a file.
	Content(ctx context.Context, f *File) ([]byte, error)
	// DeleteKind deletes every file of kind with its content and returns
	// how many were deleted.
	DeleteKind(ctx context.Context, kind Kind) (int, error)
}
