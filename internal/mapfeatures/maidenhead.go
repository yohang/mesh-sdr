package mapfeatures

import (
	"math"
	"strings"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrInvalidLocator is a string that is not a Maidenhead locator.
var ErrInvalidLocator = shared.NewError(shared.KindInvalid, "invalid_locator", "invalid Maidenhead locator")

// Square is the area of a Maidenhead locator, in decimal degrees.
type Square struct {
	Locator                  string
	South, West, North, East float64
}

// Center returns the centre of the square.
func (s Square) Center() (lat, lon float64) {
	return (s.South + s.North) / 2, (s.West + s.East) / 2
}

// The four pairs of a locator: field (A-R, 20° × 10°), square (0-9, 2° ×
// 1°), subsquare (A-X, 5' × 2.5') and extended square (0-9, 30" × 15").
var pairs = []struct {
	base     int
	lon, lat float64
	letter   bool
}{
	{18, 20, 10, true},
	{10, 2, 1, false},
	{24, 2.0 / 24, 1.0 / 24, true},
	{10, 2.0 / 240, 1.0 / 240, false},
}

// ParseLocator decodes a locator of 2, 4, 6 or 8 characters (case
// insensitive). The returned locator is normalised: field letters upper
// case, subsquare letters lower case.
func ParseLocator(s string) (Square, error) {
	if n := len(s); n == 0 || n > 8 || n%2 != 0 {
		return Square{}, ErrInvalidLocator
	}

	var (
		b    strings.Builder
		west = -180.0
		sout = -90.0
		lonW = 360.0
		latH = 180.0
	)

	for i := 0; i < len(s); i += 2 {
		p := pairs[i/2]

		x, ok1 := digit(s[i], p.base, p.letter)
		y, ok2 := digit(s[i+1], p.base, p.letter)

		if !ok1 || !ok2 {
			return Square{}, ErrInvalidLocator
		}

		west += float64(x) * p.lon
		sout += float64(y) * p.lat
		lonW, latH = p.lon, p.lat

		switch {
		case !p.letter:
			b.WriteByte(byte('0' + x))
			b.WriteByte(byte('0' + y))
		case i == 0:
			b.WriteByte(byte('A' + x))
			b.WriteByte(byte('A' + y))
		default:
			b.WriteByte(byte('a' + x))
			b.WriteByte(byte('a' + y))
		}
	}

	return Square{Locator: b.String(), South: sout, West: west, North: sout + latH, East: west + lonW}, nil
}

// digit decodes one character of a pair.
func digit(c byte, base int, letter bool) (int, bool) {
	var v int

	switch {
	case letter && c >= 'A' && c <= 'Z':
		v = int(c - 'A')
	case letter && c >= 'a' && c <= 'z':
		v = int(c - 'a')
	case !letter && c >= '0' && c <= '9':
		v = int(c - '0')
	default:
		return 0, false
	}

	return v, v < base
}

// Locator encodes a position as a locator of chars characters (2, 4, 6 or
// 8; other values are clamped and rounded down to even). The position is
// clamped to the valid range.
func Locator(lat, lon float64, chars int) string {
	chars = max(2, min(8, chars)) &^ 1

	if math.IsNaN(lat) || math.IsNaN(lon) {
		lat, lon = 0, 0
	}

	// Positions on the north or east edge belong to the last square.
	x := math.Min(math.Max(lon+180, 0), 360-1e-9)
	y := math.Min(math.Max(lat+90, 0), 180-1e-9)

	var b strings.Builder

	for i := 0; i < chars; i += 2 {
		p := pairs[i/2]
		cx := min(int(x/p.lon), p.base-1)
		cy := min(int(y/p.lat), p.base-1)
		x -= float64(cx) * p.lon
		y -= float64(cy) * p.lat

		switch {
		case !p.letter:
			b.WriteByte(byte('0' + cx))
			b.WriteByte(byte('0' + cy))
		case i == 0:
			b.WriteByte(byte('A' + cx))
			b.WriteByte(byte('A' + cy))
		default:
			b.WriteByte(byte('a' + cx))
			b.WriteByte(byte('a' + cy))
		}
	}

	return b.String()
}
