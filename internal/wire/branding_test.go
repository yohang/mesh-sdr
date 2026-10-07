package wire_test

import (
	"bytes"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// multipartBody returns a body with the fields then a file part.
func multipartBody(t *testing.T, data []byte, fields map[string]string) (string, string) {
	t.Helper()

	var buf bytes.Buffer

	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}

	fw, err := mw.CreateFormFile("file", "upload.png")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = fw.Write(data)
	_ = mw.Close()

	return mw.FormDataContentType(), buf.String()
}

// TestReceiverImages: images are uploaded on Admin › Site and served by
// GET /api/v1/branding/{slot}.
func TestReceiverImages(t *testing.T) {
	h := newAdminHub(t, nil)
	admin, anon := h.browser("root"), h.browser("")
	htmx := map[string]string{"HX-Request": "true"}

	if res, _ := anon.do(http.MethodGet, "/api/v1/branding/avatar", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unset avatar = %d", res.StatusCode)
	}

	ctype, body := multipartBody(t, pngImage(t, 48, 48), map[string]string{"slot": "avatar"})

	if res, _ := h.browser("lis").do(http.MethodPost, "/admin/site/images", ctype, body, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener upload = %d", res.StatusCode)
	}

	if res, raw := admin.do(http.MethodPost, "/admin/site/images", ctype, body, htmx); res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "The avatar was updated.") {
		t.Fatalf("upload = %d %s", res.StatusCode, raw)
	}

	res, img := anon.do(http.MethodGet, "/api/v1/branding/avatar", "", "", nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!bytes.HasPrefix(img, []byte("\x89PNG")) {
		t.Fatalf("GET = %d %v", res.StatusCode, res.Header)
	}

	if res, _ := anon.do(http.MethodGet, "/api/v1/branding/avatar", "", "", map[string]string{"If-None-Match": res.Header.Get("ETag")}); res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match = %d", res.StatusCode)
	}

	// Too large, by the declared size, before reading.
	big, bigBody := multipartBody(t, make([]byte, 3<<20), map[string]string{"slot": "avatar"})
	if res, raw := admin.do(http.MethodPost, "/admin/site/images", big, bigBody, htmx); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d %s", res.StatusCode, raw)
	}

	svgType, svgBody := multipartBody(t, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), map[string]string{"slot": "panorama"})
	if res, raw := admin.do(http.MethodPost, "/admin/site/images", svgType, svgBody, htmx); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("svg = %d %s", res.StatusCode, raw)
	}

	// The API takes JSON bodies only.
	if res, _ := admin.do(http.MethodPost, "/api/v1/auth/token", ctype, body, nil); res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("multipart POST /auth/token = %d", res.StatusCode)
	}

	if res, raw := admin.do(http.MethodPost, "/admin/site/images/remove", "application/x-www-form-urlencoded", "slot=avatar", htmx); res.StatusCode != http.StatusOK ||
		!strings.Contains(string(raw), "removed") {
		t.Errorf("remove = %d %s", res.StatusCode, raw)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action LIKE 'receiver_image.%'"); n != 2 {
		t.Errorf("audit rows = %d", n)
	}
}

func TestAdminImagesSection(t *testing.T) {
	h := newAdminHub(t, nil)
	b := h.browser("root")

	ctype, body := multipartBody(t, pngImage(t, 40, 20), map[string]string{"slot": "panorama"})

	res, raw := b.do(http.MethodPost, "/admin/site/images", ctype, body, map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "The panorama was updated.") || !strings.Contains(string(raw), `src="/api/v1/branding/panorama?v=`) {
		t.Fatalf("upload = %d %s", res.StatusCode, raw)
	}

	if _, page := b.do(http.MethodGet, "/admin/site", "", "", nil); !strings.Contains(string(page), `alt="Current Panorama"`) {
		t.Error("site page without the panorama")
	}

	res, raw = b.do(http.MethodPost, "/admin/site/images/remove", "application/x-www-form-urlencoded", "slot=panorama", map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "removed") {
		t.Errorf("remove = %d %s", res.StatusCode, raw)
	}

	if res, _ := h.browser("op").do(http.MethodPost, "/admin/site/images", ctype, body, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator upload = %d", res.StatusCode)
	}
}
