package files_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/yohang/mesh-sdr/internal/files"
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
		want files.MIMEType
	}{
		{[]byte("\x89PNG\r\n\x1a\nrest"), files.MIMEPNG},
		{[]byte{0xff, 0xd8, 0xff, 0xe0}, files.MIMEJPEG},
		{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), files.MIMEWebP},
	} {
		if got, err := files.Sniff(tt.data); err != nil || got != tt.want {
			t.Errorf("sniff %q = %s, %v", tt.data[:4], got, err)
		}
	}

	for _, bad := range [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), []byte("GIF89a...."), nil, []byte("RIFF")} {
		if _, err := files.Sniff(bad); !errors.Is(err, files.ErrUnsupportedImage) {
			t.Errorf("sniff %q: %v", bad, err)
		}
	}
}

func TestReencode(t *testing.T) {
	p := files.NewProcessor()
	ctx := context.Background()
	big := files.MaxPanoramaPixels

	img, err := p.Reencode(ctx, pngOf(t, 20, 10), files.MIMEJPEG, big)
	if err != nil || img.MIME != files.MIMEJPEG || img.Width != 20 || img.Height != 10 || !bytes.HasPrefix(img.Data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("png → jpeg = %+v, %v", img.MIME, err)
	}

	src := jpegWithEXIF(t)
	if !bytes.Contains(src, []byte("GPSLatitude")) {
		t.Fatal("fixture has no EXIF")
	}

	img, err = p.Reencode(ctx, src, files.MIMEPNG, big)
	if err != nil || img.MIME != files.MIMEPNG || bytes.Contains(img.Data, []byte("GPSLatitude")) || bytes.Contains(img.Data, []byte("Exif")) {
		t.Errorf("metadata kept or error: %v", err)
	}

	if _, err := p.Reencode(ctx, pngOf(t, files.MaxImageSide+1, 1), files.MIMEPNG, big); !errors.Is(err, files.ErrImageDimensions) {
		t.Errorf("too wide: %v", err)
	}

	// Pixel cap of the slot, read before decoding.
	if _, err := p.Reencode(ctx, pngOf(t, 1025, 1024), files.MIMEPNG, files.MaxAvatarPixels); !errors.Is(err, files.ErrImageDimensions) {
		t.Errorf("avatar over 1024²: %v", err)
	}

	// 16-bit images are refused before decoding.
	var buf16 bytes.Buffer
	if err := png.Encode(&buf16, image.NewRGBA64(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Reencode(ctx, buf16.Bytes(), files.MIMEPNG, big); !errors.Is(err, files.ErrImageColorModel) {
		t.Errorf("16-bit: %v", err)
	}

	truncated := pngOf(t, 4, 4)[:30]
	if _, err := p.Reencode(ctx, truncated, files.MIMEPNG, big); !errors.Is(err, files.ErrUnsupportedImage) {
		t.Errorf("truncated: %v", err)
	}
}
