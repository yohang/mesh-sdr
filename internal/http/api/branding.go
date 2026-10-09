package api

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/files"
)

// BrandingService reads the receiver images (ADM-004).
type BrandingService interface {
	Current(ctx context.Context, slot files.Slot) (*files.File, error)
	Content(ctx context.Context, slot files.Slot) (*files.File, []byte, error)
}

// BrandingHandlers serve GET /branding/{slot}, the images the pages show.
// They are changed on Admin › Site.
type BrandingHandlers struct {
	branding BrandingService
}

// NewBrandingHandlers returns the handlers.
func NewBrandingHandlers(b BrandingService) BrandingHandlers {
	return BrandingHandlers{branding: b}
}

// GetReceiverImage implements StrictServerInterface.
func (h BrandingHandlers) GetReceiverImage(ctx context.Context, req GetReceiverImageRequestObject) (GetReceiverImageResponseObject, error) {
	slot, err := files.ParseSlot(req.Slot)
	if err != nil {
		return nil, err
	}

	// Compare the ETag on the metadata before loading the content.
	f, err := h.branding.Current(ctx, slot)
	if err != nil {
		return nil, err
	}

	sum := f.SHA256()
	etag := strconv.Quote(hex.EncodeToString(sum[:]))

	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == etag {
		return imageResponse{fileResponse{etag: etag, public: true, notModified: true}}, nil
	}

	f, data, err := h.branding.Content(ctx, slot)
	if err != nil {
		return nil, err
	}

	sum = f.SHA256()

	return imageResponse{fileResponse{
		etag: strconv.Quote(hex.EncodeToString(sum[:])), mime: string(f.MIME()), name: f.Name(), disposition: "inline",
		public: true, size: int64(len(data)), stream: func(w io.Writer) error {
			_, err := w.Write(data)

			return err
		},
	}}, nil
}

// imageResponse is a receiver image, shown inline.
type imageResponse struct{ fileResponse }

func (r imageResponse) VisitGetReceiverImageResponse(w http.ResponseWriter) error { return r.write(w) }
