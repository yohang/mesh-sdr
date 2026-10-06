package web_test

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// colorToken matches a color token definition: --msdr-color-x: light-dark(#light, #dark);
var colorToken = regexp.MustCompile(`(?m)^\s*--msdr-color-([a-z-]+):\s*light-dark\(\s*(#[0-9a-fA-F]{6})\s*,\s*(#[0-9a-fA-F]{6})\s*\);`)

// tokens returns the light and dark value of every color token in input.css.
func tokens(t *testing.T) (light, dark map[string]string) {
	t.Helper()

	css, err := os.ReadFile("static/css/input.css")
	if err != nil {
		t.Fatal(err)
	}

	light, dark = map[string]string{}, map[string]string{}

	for _, m := range colorToken.FindAllStringSubmatch(string(css), -1) {
		light[m[1]], dark[m[1]] = m[2], m[3]
	}

	return light, dark
}

// luminance is the WCAG 2.1 relative luminance of a #rrggbb color.
func luminance(t *testing.T, hex string) float64 {
	t.Helper()

	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		t.Fatalf("color %q: %v", hex, err)
	}

	channel := func(c uint64) float64 {
		s := float64(c) / 255
		if s <= 0.04045 {
			return s / 12.92
		}

		return math.Pow((s+0.055)/1.055, 2.4)
	}

	return 0.2126*channel(v>>16&0xff) + 0.7152*channel(v>>8&0xff) + 0.0722*channel(v&0xff)
}

func contrast(t *testing.T, a, b string) float64 {
	t.Helper()

	la, lb := luminance(t, a), luminance(t, b)

	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

// TestTokenContrast enforces WCAG 2.1 AA on the token pairs the UI uses
// (UI-008, UI-009): 4.5:1 for text (1.4.3), 3:1 for UI components and focus
// indicators (1.4.11), in both the light and the dark set.
func TestTokenContrast(t *testing.T) {
	light, dark := tokens(t)

	backgrounds := []string{"bg", "surface", "surface-raised"}
	text := []string{"fg", "fg-muted", "accent", "danger", "warning", "success", "info"}
	ui := []string{"border", "focus", "accent"}

	type pair struct {
		fg, bg string
		min    float64
	}

	var pairs []pair

	for _, bg := range backgrounds {
		for _, fg := range text {
			pairs = append(pairs, pair{fg, bg, 4.5})
		}

		for _, fg := range ui {
			pairs = append(pairs, pair{fg, bg, 3})
		}
	}

	pairs = append(pairs, pair{"accent-fg", "accent", 4.5})

	for name, set := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range pairs {
			t.Run(fmt.Sprintf("%s/%s-on-%s", name, p.fg, p.bg), func(t *testing.T) {
				fg, ok1 := set[p.fg]
				bg, ok2 := set[p.bg]

				if !ok1 || !ok2 {
					t.Fatalf("missing token --msdr-color-%s or --msdr-color-%s", p.fg, p.bg)
				}

				if c := contrast(t, fg, bg); c < p.min {
					t.Errorf("%s on %s = %.2f:1, want ≥ %.1f:1", fg, bg, c, p.min)
				}
			})
		}
	}
}

// TestThemeColors checks that the browser UI colors (theme-color, manifest)
// are the surface token of each set.
func TestThemeColors(t *testing.T) {
	light, dark := tokens(t)

	if light["surface"] != layout.ThemeColorLight || dark["surface"] != layout.ThemeColorDark {
		t.Errorf("theme colors %s/%s, surface token %s/%s",
			layout.ThemeColorLight, layout.ThemeColorDark, light["surface"], dark["surface"])
	}
}

// TestTokenInitialValues checks that each @property initial value is the
// light value of its token, so both stay in sync.
func TestTokenInitialValues(t *testing.T) {
	css, err := os.ReadFile("static/css/input.css")
	if err != nil {
		t.Fatal(err)
	}

	light, _ := tokens(t)

	props := regexp.MustCompile(`@property --msdr-color-([a-z-]+) \{[^}]*initial-value: (#[0-9a-fA-F]{6});`).FindAllStringSubmatch(string(css), -1)
	if len(props) != len(light) {
		t.Errorf("%d @property rules for %d color tokens", len(props), len(light))
	}

	for _, m := range props {
		if light[m[1]] != m[2] {
			t.Errorf("@property --msdr-color-%s initial-value %s, light value %s", m[1], m[2], light[m[1]])
		}
	}
}
