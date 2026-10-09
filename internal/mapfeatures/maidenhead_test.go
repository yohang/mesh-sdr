package mapfeatures

import (
	"errors"
	"math"
	"testing"
)

func TestParseLocator(t *testing.T) {
	for _, tc := range []struct {
		in, want                 string
		south, west, north, east float64
	}{
		{"JN", "JN", 40, 0, 50, 20},
		{"jn18", "JN18", 48, 2, 49, 4},
		{"JO62", "JO62", 52, 12, 53, 14},
		{"JN18EU", "JN18eu", 48 + 20.0/24, 2 + 4*2.0/24, 48 + 21.0/24, 2 + 5*2.0/24},
		{"AA00aa00", "AA00aa00", -90, -180, -90 + 1.0/240, -180 + 2.0/240},
		{"RR99xx99", "RR99xx99", 90 - 1.0/240, 180 - 2.0/240, 90, 180},
	} {
		sq, err := ParseLocator(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}

		if sq.Locator != tc.want || !near(sq.South, tc.south) || !near(sq.West, tc.west) || !near(sq.North, tc.north) || !near(sq.East, tc.east) {
			t.Errorf("%s = %+v, want %s %g %g %g %g", tc.in, sq, tc.want, tc.south, tc.west, tc.north, tc.east)
		}
	}

	for _, bad := range []string{"", "J", "JN1", "SA00", "JNAA", "JN18zz", "JN18eu0", "JN18eu001", "JN18eu0a", "J N18"} {
		if _, err := ParseLocator(bad); !errors.Is(err, ErrInvalidLocator) {
			t.Errorf("%q: %v, want ErrInvalidLocator", bad, err)
		}
	}
}

func TestLocator(t *testing.T) {
	for _, tc := range []struct {
		lat, lon float64
		chars    int
		want     string
	}{
		{48.8566, 2.3522, 6, "JN18eu"}, // Paris
		{48.8566, 2.3522, 4, "JN18"},
		{48.8566, 2.3522, 8, "JN18eu25"},
		{48.8566, 2.3522, 3, "JN"},
		{50.63, 3.06, 4, "JO10"},          // Lille
		{-33.8688, 151.2093, 6, "QF56od"}, // Sydney
		{40.7128, -74.006, 4, "FN20"},     // New York
		{90, 180, 4, "RR99"},
		{-90, -180, 4, "AA00"},
		{math.NaN(), 0, 2, "JJ"},
	} {
		if got := Locator(tc.lat, tc.lon, tc.chars); got != tc.want {
			t.Errorf("Locator(%g, %g, %d) = %s, want %s", tc.lat, tc.lon, tc.chars, got, tc.want)
		}
	}
}

// A locator's centre encodes back to the same locator.
func TestLocatorRoundTrip(t *testing.T) {
	for _, loc := range []string{"JN18", "JN18eu", "AA00aa00", "RR99xx99", "FN20xr", "QF56od"} {
		sq, err := ParseLocator(loc)
		if err != nil {
			t.Fatal(err)
		}

		lat, lon := sq.Center()
		if got := Locator(lat, lon, len(loc)); got != loc {
			t.Errorf("%s: centre %g,%g encodes to %s", loc, lat, lon, got)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
