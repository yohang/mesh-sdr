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

	xdraw "golang.org/x/image/draw"
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
	img, err := p.decode(ctx, data, maxPixels)
	if err != nil {
		return Image{}, err
	}

	return encode(img, out)
}

// ReencodeWithThumbnail re-encodes an image like Reencode and also returns
// a JPEG thumbnail whose longest side is at most side pixels.
func (p *Processor) ReencodeWithThumbnail(ctx context.Context, data []byte, out MIMEType, maxPixels, side int) (Image, Image, error) {
	img, err := p.decode(ctx, data, maxPixels)
	if err != nil {
		return Image{}, Image{}, err
	}

	full, err := encode(img, out)
	if err != nil {
		return Image{}, Image{}, err
	}

	thumb, err := encode(scaleDown(img, side), MIMEJPEG)
	if err != nil {
		return Image{}, Image{}, err
	}

	return full, thumb, nil
}

// decode checks an image's type, dimensions and colour model before
// decoding it, one image at a time.
func (p *Processor) decode(ctx context.Context, data []byte, maxPixels int) (image.Image, error) {
	t, err := Sniff(data)
	if err != nil {
		return nil, err
	}

	c := codecOf(t)

	cfg, err := c.config(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupportedImage.WithDetail("the image cannot be read")
	}

	switch {
	case cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxImageSide || cfg.Height > MaxImageSide:
		return nil, ErrImageDimensions
	case cfg.Width*cfg.Height > maxPixels:
		return nil, ErrImageDimensions.WithDetail(fmt.Sprintf("the image has %d × %d pixels; at most %d pixels are accepted",
			cfg.Width, cfg.Height, maxPixels))
	case !supportedModel(cfg.ColorModel):
		return nil, ErrImageColorModel
	}

	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	img, err := c.decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupportedImage.WithDetail("the image cannot be read")
	}

	return img, nil
}

// encode writes img as out, without any metadata.
func encode(img image.Image, out MIMEType) (Image, error) {
	var (
		buf bytes.Buffer
		err error
	)

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

// scaleDown returns img scaled so that its longest side is at most side
// pixels (img itself when it is small enough).
func scaleDown(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	if w <= side && h <= side {
		return img
	}

	if w >= h {
		w, h = side, max(1, h*side/w)
	} else {
		w, h = max(1, w*side/h), side
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)

	return dst
}

// flatten draws an image on white: JPEG has no transparency.
func flatten(img image.Image) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)

	return dst
}
