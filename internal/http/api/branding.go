package api

import (
	"bytes"
	"context"
	"encoding/hex"
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
		return imageResponse{file: f, etag: etag, notModified: true}, nil
	}

	f, data, err := h.branding.Content(ctx, slot)
	if err != nil {
		return nil, err
	}

	sum = f.SHA256()

	return imageResponse{file: f, data: data, etag: strconv.Quote(hex.EncodeToString(sum[:]))}, nil
}

type imageResponse struct {
	file        *files.File
	data        []byte
	etag        string
	notModified bool
}

func (r imageResponse) VisitGetReceiverImageResponse(w http.ResponseWriter) error {
	h := w.Header()
	h.Set("ETag", r.etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")

	if r.notModified {
		w.WriteHeader(http.StatusNotModified)

		return nil
	}

	h.Set("Content-Type", string(r.file.MIME()))
	h.Set("Content-Length", strconv.Itoa(len(r.data)))
	h.Set("Content-Disposition", `inline; filename="`+r.file.Name()+`"`)
	w.WriteHeader(http.StatusOK)

	_, err := bytes.NewReader(r.data).WriteTo(w)

	return err
}
