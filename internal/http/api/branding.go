package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/yohang/mesh-sdr/internal/files/app"
	"github.com/yohang/mesh-sdr/internal/files/domain"
	fileshttp "github.com/yohang/mesh-sdr/internal/files/http"
	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// BrandingService is the receiver images use case (ADM-004).
type BrandingService interface {
	Upload(ctx context.Context, actor app.Actor, slot domain.Slot, data []byte) (*domain.File, error)
	Remove(ctx context.Context, actor app.Actor, slot domain.Slot) (bool, error)
	Content(ctx context.Context, slot domain.Slot) (*domain.File, []byte, error)
}

// BrandingHandlers serve /branding/{slot}.
type BrandingHandlers struct {
	branding BrandingService
	actor    ActorFunc
}

// NewBrandingHandlers returns the handlers.
func NewBrandingHandlers(b BrandingService, actor ActorFunc) BrandingHandlers {
	return BrandingHandlers{branding: b, actor: actor}
}

func (h BrandingHandlers) filesActor(ctx context.Context) app.Actor {
	a := h.actor(ctx)

	return app.Actor{User: a.User, IP: a.IP, RequestID: a.RequestID}
}

// GetReceiverImage implements StrictServerInterface.
func (h BrandingHandlers) GetReceiverImage(ctx context.Context, req GetReceiverImageRequestObject) (GetReceiverImageResponseObject, error) {
	slot, err := domain.ParseSlot(req.Slot)
	if err != nil {
		return nil, err
	}

	f, data, err := h.branding.Content(ctx, slot)
	if err != nil {
		return nil, err
	}

	sum := f.SHA256()
	etag := strconv.Quote(hex.EncodeToString(sum[:]))

	return imageResponse{file: f, data: data, etag: etag, notModified: req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == etag}, nil
}

// PutReceiverImage implements StrictServerInterface.
func (h BrandingHandlers) PutReceiverImage(ctx context.Context, req PutReceiverImageRequestObject) (PutReceiverImageResponseObject, error) {
	slot, err := domain.ParseSlot(req.Slot)
	if err != nil {
		return nil, err
	}

	_, data, err := fileshttp.ReadUpload(req.Body, func(map[string]string) (domain.Slot, error) { return slot, nil })

	switch {
	case errors.Is(err, fileshttp.ErrTooLarge):
		return problemResponse(problem.New(http.StatusRequestEntityTooLarge, string(domain.ErrImageTooLarge.Code()),
			"the "+slot.Name()+" must not exceed "+strconv.FormatInt(slot.MaxUpload()>>10, 10)+" KiB")), nil
	case err != nil:
		return problemResponse(problem.New(http.StatusBadRequest, problem.CodeBadRequest, "a multipart body with a file part is required")), nil
	}

	f, err := h.branding.Upload(ctx, h.filesActor(ctx), slot, data)
	if err != nil {
		return nil, err
	}

	sum := f.SHA256()

	return PutReceiverImage200JSONResponse{
		Slot: ReceiverImageSlot(slot.Name()), Name: f.Name(), MimeType: string(f.MIME()), SizeBytes: f.Size(),
		Width: f.Width(), Height: f.Height(), Sha256: hex.EncodeToString(sum[:]),
	}, nil
}

// DeleteReceiverImage implements StrictServerInterface.
func (h BrandingHandlers) DeleteReceiverImage(ctx context.Context, req DeleteReceiverImageRequestObject) (DeleteReceiverImageResponseObject, error) {
	slot, err := domain.ParseSlot(req.Slot)
	if err != nil {
		return nil, err
	}

	if _, err := h.branding.Remove(ctx, h.filesActor(ctx), slot); err != nil {
		return nil, err
	}

	return DeleteReceiverImage204Response{}, nil
}

type imageResponse struct {
	file        *domain.File
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

// problemResponse writes a problem from a strict handler.
type problemResponse problem.Problem

func (p problemResponse) VisitPutReceiverImageResponse(w http.ResponseWriter) error {
	problem.Write(w, problem.Problem(p))

	return nil
}

// uploadLimits bounds the bodies of the image uploads before routing: the
// declared size is checked before anything is read (413), and the body is
// capped (TECHNICAL_SPEC §7.3 "File blobs in the DB").
func uploadLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if name, ok := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1"), "/branding/"); ok {
				if slot, err := domain.ParseSlot(name); err == nil {
					if fileshttp.CheckDeclaredSize(r, slot) != nil {
						problem.Write(w, problem.New(http.StatusRequestEntityTooLarge, string(domain.ErrImageTooLarge.Code()),
							"the "+slot.Name()+" must not exceed "+strconv.FormatInt(slot.MaxUpload()>>10, 10)+" KiB"))

						return
					}

					fileshttp.LimitBody(w, r, slot)
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

var (
	multipartOnce sync.Once
	multipartOps  []multipartOp
)

type multipartOp struct {
	method string
	path   *regexp.Regexp
}

// AcceptsMultipart reports whether r targets an operation whose request
// body is declared multipart/form-data in openapi.yaml (uploads): the
// JSON-only rule of /api/v1 does not apply to it.
func AcceptsMultipart(r *http.Request) bool {
	multipartOnce.Do(func() { multipartOps = loadMultipartOps(specJSON) })

	for _, op := range multipartOps {
		if op.method == r.Method && op.path.MatchString(r.URL.Path) {
			return true
		}
	}

	return false
}

func loadMultipartOps(spec []byte) []multipartOp {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}

	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil
	}

	var out []multipartOp

	for path, item := range doc.Paths {
		for method, raw := range item {
			var op struct {
				RequestBody struct {
					Content map[string]json.RawMessage `json:"content"`
				} `json:"requestBody"`
			}

			if json.Unmarshal(raw, &op) != nil {
				continue
			}

			if _, ok := op.RequestBody.Content["multipart/form-data"]; !ok {
				continue
			}

			segments := strings.Split(path, "/")
			for i, seg := range segments {
				if strings.HasPrefix(seg, "{") {
					segments[i] = `[^/]+`
				} else {
					segments[i] = regexp.QuoteMeta(seg)
				}
			}

			pattern := "^/api/v1" + strings.Join(segments, "/") + "$"
			out = append(out, multipartOp{method: strings.ToUpper(method), path: regexp.MustCompile(pattern)})
		}
	}

	return out
}
