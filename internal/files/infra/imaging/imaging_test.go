package imaging_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/yohang/mesh-sdr/internal/files/domain"
	"github.com/yohang/mesh-sdr/internal/files/infra/imaging"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{R: 200, A: 128})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// jpegWithEXIF returns a JPEG carrying an APP1 Exif segment with a GPS tag
// marker, inserted right after SOI.
func jpegWithEXIF(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 8)), nil); err != nil {
		t.Fatal(err)
	}

	payload := append([]byte("Exif\x00\x00"), []byte("GPSLatitude 50.63")...)
	seg := append([]byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	b := buf.Bytes()

	return append(append(append([]byte{}, b[:2]...), seg...), b[2:]...)
}

func TestSniff(t *testing.T) {
	for _, tt := range []struct {
		data []byte
		want domain.MIMEType
	}{
		{[]byte("\x89PNG\r\n\x1a\nrest"), domain.MIMEPNG},
		{[]byte{0xff, 0xd8, 0xff, 0xe0}, domain.MIMEJPEG},
		{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), domain.MIMEWebP},
	} {
		if got, err := imaging.Sniff(tt.data); err != nil || got != tt.want {
			t.Errorf("sniff %q = %s, %v", tt.data[:4], got, err)
		}
	}

	for _, bad := range [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), []byte("GIF89a...."), nil, []byte("RIFF")} {
		if _, err := imaging.Sniff(bad); !errors.Is(err, domain.ErrUnsupportedImage) {
			t.Errorf("sniff %q: %v", bad, err)
		}
	}
}

func TestReencode(t *testing.T) {
	p := imaging.Processor{}

	img, err := p.Reencode(pngOf(t, 20, 10), domain.MIMEJPEG)
	if err != nil || img.MIME != domain.MIMEJPEG || img.Width != 20 || img.Height != 10 || !bytes.HasPrefix(img.Data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("png → jpeg = %+v, %v", img.MIME, err)
	}

	src := jpegWithEXIF(t)
	if !bytes.Contains(src, []byte("GPSLatitude")) {
		t.Fatal("fixture has no EXIF")
	}

	img, err = p.Reencode(src, domain.MIMEPNG)
	if err != nil || img.MIME != domain.MIMEPNG || bytes.Contains(img.Data, []byte("GPSLatitude")) || bytes.Contains(img.Data, []byte("Exif")) {
		t.Errorf("metadata kept or error: %v", err)
	}

	if _, err := p.Reencode(pngOf(t, domain.MaxImageSide+1, 1), domain.MIMEPNG); !errors.Is(err, domain.ErrImageDimensions) {
		t.Errorf("too wide: %v", err)
	}

	truncated := pngOf(t, 4, 4)[:30]
	if _, err := p.Reencode(truncated, domain.MIMEPNG); !errors.Is(err, domain.ErrUnsupportedImage) {
		t.Errorf("truncated: %v", err)
	}
}
