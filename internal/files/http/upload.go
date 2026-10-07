// Package http serves the receiver images of the admin Site page (ADM-004):
// the images section, its upload and "restore default" actions. The images
// themselves are served by GET /api/v1/branding/{slot} (internal/http/api).
package http

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/files/domain"
)

// multipartOverhead is the room left for the multipart framing and the
// other form fields around the file part.
const multipartOverhead = 64 << 10

// ErrTooLarge is an upload over the slot's limit (HTTP 413).
var ErrTooLarge = errors.New("upload too large")

// CheckDeclaredSize refuses a request whose declared body is larger than an
// upload to slot can be, before anything is read.
func CheckDeclaredSize(r *http.Request, slot domain.Slot) error {
	if r.ContentLength > slot.MaxUpload()+multipartOverhead {
		return ErrTooLarge
	}

	return nil
}

// LimitBody bounds the body of an upload to slot.
func LimitBody(w http.ResponseWriter, r *http.Request, slot domain.Slot) {
	r.Body = http.MaxBytesReader(w, r.Body, slot.MaxUpload()+multipartOverhead)
}

// ReadUpload reads the "file" part of a multipart body, at most
// slot.MaxUpload() bytes (ErrTooLarge beyond). Other parts are skipped;
// fields collects the small text parts (for example "slot").
func ReadUpload(mr *multipart.Reader, slot func(fields map[string]string) (domain.Slot, error)) (domain.Slot, []byte, error) {
	fields := map[string]string{}

	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return domain.Slot{}, nil, errors.New("no file part")
		}

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return domain.Slot{}, nil, ErrTooLarge
		}

		if err != nil {
			return domain.Slot{}, nil, fmt.Errorf("read multipart body: %w", err)
		}

		if p.FormName() != "file" {
			b, err := io.ReadAll(io.LimitReader(p, 256))
			if err != nil {
				return domain.Slot{}, nil, fmt.Errorf("read form field: %w", err)
			}

			fields[p.FormName()] = string(b)

			continue
		}

		s, err := slot(fields)
		if err != nil {
			return domain.Slot{}, nil, err
		}

		data, err := io.ReadAll(io.LimitReader(p, s.MaxUpload()+1))
		if errors.As(err, &tooLarge) || int64(len(data)) > s.MaxUpload() {
			return s, nil, ErrTooLarge
		}

		if err != nil {
			return s, nil, fmt.Errorf("read file part: %w", err)
		}

		return s, data, nil
	}
}
