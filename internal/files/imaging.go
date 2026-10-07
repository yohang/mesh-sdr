package files

// The image processor validates and re-encodes uploaded images with the
// standard library and golang.org/x/image/webp (TECHNICAL_SPEC §7.3 "File
// blobs in the DB" rule 3): the type is taken from the magic bytes, the
// dimensions are read before decoding, and re-encoding drops every metadata
// block (EXIF, GPS, comments).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

// JPEGQuality is the quality of re-encoded JPEG images.
const JPEGQuality = 85

// Processor implements ImageProcessor. It decodes one image at a time:
// a decode may take hundreds of megabytes and seconds of CPU.
type Processor struct{ slot chan struct{} }

// NewProcessor returns a processor.
func NewProcessor() *Processor { return &Processor{slot: make(chan struct{}, 1)} }

// Sniff returns the type of an image from its magic bytes: PNG, JPEG or
// WebP. Anything else (SVG, GIF, …) is refused.
func Sniff(b []byte) (MIMEType, error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG, nil
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return MIMEJPEG, nil
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return MIMEWebP, nil
	default:
		return "", ErrUnsupportedImage
	}
}

type codec struct {
	config func(r *bytes.Reader) (image.Config, error)
	decode func(r *bytes.Reader) (image.Image, error)
}

func codecOf(t MIMEType) codec {
	switch t {
	case MIMEPNG:
		return codec{func(r *bytes.Reader) (image.Config, error) { return png.DecodeConfig(r) }, func(r *bytes.Reader) (image.Image, error) { return png.Decode(r) }}
	case MIMEJPEG:
		return codec{func(r *bytes.Reader) (image.Config, error) { return jpeg.DecodeConfig(r) }, func(r *bytes.Reader) (image.Image, error) { return jpeg.Decode(r) }}
	default:
		return codec{func(r *bytes.Reader) (image.Config, error) { return webp.DecodeConfig(r) }, func(r *bytes.Reader) (image.Image, error) { return webp.Decode(r) }}
	}
}

// supportedModel reports 8-bit colour models; 16-bit models double the
// decoding memory and are refused.
func supportedModel(m color.Model) bool {
	switch m {
	case color.RGBAModel, color.NRGBAModel, color.GrayModel, color.YCbCrModel, color.NYCbCrAModel, color.CMYKModel, color.AlphaModel:
		return true
	}

	_, palette := m.(color.Palette)

	return palette
}

// Reencode implements ImageProcessor.
func (p *Processor) Reencode(ctx context.Context, data []byte, out MIMEType, maxPixels int) (Image, error) {
	t, err := Sniff(data)
	if err != nil {
		return Image{}, err
	}

	c := codecOf(t)

	cfg, err := c.config(bytes.NewReader(data))
	if err != nil {
		return Image{}, ErrUnsupportedImage.WithDetail("the image cannot be read")
	}

	switch {
	case cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxImageSide || cfg.Height > MaxImageSide:
		return Image{}, ErrImageDimensions
	case cfg.Width*cfg.Height > maxPixels:
		return Image{}, ErrImageDimensions.WithDetail(fmt.Sprintf("the image has %d × %d pixels; at most %d pixels are accepted",
			cfg.Width, cfg.Height, maxPixels))
	case !supportedModel(cfg.ColorModel):
		return Image{}, ErrImageColorModel
	}

	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	case <-ctx.Done():
		return Image{}, ctx.Err()
	}

	img, err := c.decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, ErrUnsupportedImage.WithDetail("the image cannot be read")
	}

	var buf bytes.Buffer

	switch out {
	case MIMEPNG:
		err = (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, img)
	case MIMEJPEG:
		err = jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: JPEGQuality})
	default:
		return Image{}, fmt.Errorf("cannot encode %s", out)
	}

	if err != nil {
		return Image{}, errors.Join(ErrUnsupportedImage, err)
	}

	b := img.Bounds()

	return Image{Data: buf.Bytes(), MIME: out, Width: b.Dx(), Height: b.Dy()}, nil
}

// flatten draws an image on white: JPEG has no transparency.
func flatten(img image.Image) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)

	return dst
}
