// Command icongen generates the app icons (UI-004) from the geometry below:
// an SVG, PNGs at the sizes the web app manifest and iOS need, and a
// multi-size favicon.ico. It runs from go:generate (internal/web), so the
// binary icons are never committed and no image tool or Node is needed.
//
// The mark is a placeholder: three mesh nodes linked in a triangle on an
// accent-colored tile.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/image/vector"
)

// Colors: the light accent token on its foreground (see static/css/input.css).
var (
	background = color.RGBA{0x0b, 0x5c, 0xad, 0xff}
	foreground = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

type point struct{ x, y float64 }

// mark is the foreground geometry in unit coordinates (0..1).
var (
	nodes      = []point{{0.5, 0.27}, {0.25, 0.70}, {0.75, 0.70}}
	nodeRadius = 0.11
	linkWidth  = 0.06
	cornerFrac = 0.22 // corner radius of the rounded tile
)

// variant is one icon layout.
type variant struct {
	rounded bool    // rounded tile with transparent corners; else full bleed
	scale   float64 // mark size within the tile (maskable icons keep it in the safe zone)
}

var (
	anyPurpose = variant{rounded: true, scale: 1}
	fullBleed  = variant{rounded: false, scale: 0.9}
	maskable   = variant{rounded: false, scale: 0.7}
)

func main() {
	out := flag.String("out", "static/icons", "output directory")
	flag.Parse()

	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

func run(out string) error {
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}

	pngs := []struct {
		name string
		size int
		v    variant
	}{
		{"icon-192.png", 192, anyPurpose},
		{"icon-512.png", 512, anyPurpose},
		{"icon-maskable-512.png", 512, maskable},
		{"apple-touch-icon.png", 180, fullBleed},
	}

	for _, p := range pngs {
		b, err := encodePNG(raster(p.size, p.v))
		if err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}

		if err := os.WriteFile(filepath.Join(out, p.name), b, 0o600); err != nil {
			return err
		}
	}

	ico, err := encodeICO(16, 32, 48)
	if err != nil {
		return fmt.Errorf("favicon.ico: %w", err)
	}

	if err := os.WriteFile(filepath.Join(out, "favicon.ico"), ico, 0o600); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(out, "icon.svg"), []byte(svg(anyPurpose)), 0o600)
}

// transform maps a unit mark coordinate into the tile for variant v.
func transform(p point, v variant) point {
	o := (1 - v.scale) / 2

	return point{o + p.x*v.scale, o + p.y*v.scale}
}

// links returns the link polygons (rectangles) between the nodes.
func links(v variant) [][]point {
	var out [][]point

	for i := range nodes {
		a, b := transform(nodes[i], v), transform(nodes[(i+1)%len(nodes)], v)
		dx, dy := b.x-a.x, b.y-a.y
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l*linkWidth*v.scale/2, dx/l*linkWidth*v.scale/2

		out = append(out, []point{{a.x + nx, a.y + ny}, {b.x + nx, b.y + ny}, {b.x - nx, b.y - ny}, {a.x - nx, a.y - ny}})
	}

	return out
}

// kappa places cubic control points to approximate a quarter circle.
const kappa = 0.5522847498

func raster(size int, v variant) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float32(size)

	fill := func(c color.RGBA, path func(z *vector.Rasterizer)) {
		z := vector.NewRasterizer(size, size)
		path(z)
		z.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{})
	}

	fill(background, func(z *vector.Rasterizer) {
		if !v.rounded {
			z.MoveTo(0, 0)
			z.LineTo(s, 0)
			z.LineTo(s, s)
			z.LineTo(0, s)
			z.ClosePath()

			return
		}

		r := float32(cornerFrac) * s
		k := r * kappa

		z.MoveTo(r, 0)
		z.LineTo(s-r, 0)
		z.CubeTo(s-r+k, 0, s, r-k, s, r)
		z.LineTo(s, s-r)
		z.CubeTo(s, s-r+k, s-r+k, s, s-r, s)
		z.LineTo(r, s)
		z.CubeTo(r-k, s, 0, s-r+k, 0, s-r)
		z.LineTo(0, r)
		z.CubeTo(0, r-k, r-k, 0, r, 0)
		z.ClosePath()
	})

	for _, poly := range links(v) {
		fill(foreground, func(z *vector.Rasterizer) {
			z.MoveTo(float32(poly[0].x)*s, float32(poly[0].y)*s)
			for _, p := range poly[1:] {
				z.LineTo(float32(p.x)*s, float32(p.y)*s)
			}
			z.ClosePath()
		})
	}

	for _, n := range nodes {
		c := transform(n, v)
		cx, cy, r := float32(c.x)*s, float32(c.y)*s, float32(nodeRadius*v.scale)*s
		k := r * kappa

		fill(foreground, func(z *vector.Rasterizer) {
			z.MoveTo(cx+r, cy)
			z.CubeTo(cx+r, cy+k, cx+k, cy+r, cx, cy+r)
			z.CubeTo(cx-k, cy+r, cx-r, cy+k, cx-r, cy)
			z.CubeTo(cx-r, cy-k, cx-k, cy-r, cx, cy-r)
			z.CubeTo(cx+k, cy-r, cx+r, cy-k, cx+r, cy)
			z.ClosePath()
		})
	}

	return img
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer

	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeICO writes an ICO file with one PNG-compressed image per size.
func encodeICO(sizes ...int) ([]byte, error) {
	images := make([][]byte, len(sizes))

	for i, size := range sizes {
		b, err := encodePNG(raster(size, anyPurpose))
		if err != nil {
			return nil, err
		}

		images[i] = b
	}

	var buf bytes.Buffer

	// ICONDIR: reserved, type 1 (icon), image count.
	header := []uint16{0, 1, uint16(len(sizes))} //nolint:gosec // a handful of sizes
	if err := binary.Write(&buf, binary.LittleEndian, header); err != nil {
		return nil, err
	}

	offset := 6 + 16*len(sizes)

	for i, size := range sizes {
		// ICONDIRENTRY: width, height, palette, reserved, planes, bpp, size, offset.
		entry := struct {
			W, H, Palette, Reserved uint8
			Planes, BPP             uint16
			Size, Offset            uint32
		}{uint8(size), uint8(size), 0, 0, 1, 32, uint32(len(images[i])), uint32(offset)} //nolint:gosec // sizes < 256, small files

		if err := binary.Write(&buf, binary.LittleEndian, entry); err != nil {
			return nil, err
		}

		offset += len(images[i])
	}

	for _, b := range images {
		buf.Write(b)
	}

	return buf.Bytes(), nil
}

// svg renders the same geometry as a 100×100 SVG.
func svg(v variant) string {
	const u = 100.0

	f := func(x float64) string { return strconv.FormatFloat(math.Round(x*u*100)/100, 'f', -1, 64) }

	var b strings.Builder

	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">`)
	fmt.Fprintf(&b, `<rect width="100" height="100" rx="%s" fill="#%02x%02x%02x"/>`, f(cornerFrac), background.R, background.G, background.B)
	fmt.Fprintf(&b, `<g fill="#%02x%02x%02x">`, foreground.R, foreground.G, foreground.B)

	for _, poly := range links(v) {
		pts := make([]string, len(poly))
		for i, p := range poly {
			pts[i] = f(p.x) + "," + f(p.y)
		}

		fmt.Fprintf(&b, `<polygon points="%s"/>`, strings.Join(pts, " "))
	}

	for _, n := range nodes {
		c := transform(n, v)
		fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="%s"/>`, f(c.x), f(c.y), f(nodeRadius*v.scale))
	}

	b.WriteString("</g></svg>\n")

	return b.String()
}
