package layout_test

import (
	"net/url"
	"testing"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

func TestFormatHz(t *testing.T) {
	for hz, want := range map[int64]string{999: "999 Hz", 7_074_000: "7.074 MHz", 145_500_000: "145.5 MHz", 12_500: "12.5 kHz"} {
		if got := layout.FormatHz(hz); got != want {
			t.Errorf("FormatHz(%d) = %q, want %q", hz, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KiB", 3 << 20: "3.0 MiB", 1 << 62: "4.0 EiB"} {
		if got := layout.HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0 bookmarks", 1: "1 bookmark", 2: "2 bookmarks"} {
		if got := layout.Count(n, "bookmark", "bookmarks"); got != want {
			t.Errorf("Count(%d) = %q", n, got)
		}
	}
}

func TestPagingURL(t *testing.T) {
	p := layout.Paging{Path: "/files", Query: url.Values{"device": {"hf"}, "page": {"7"}}}

	for n, want := range map[int]string{1: "/files?device=hf", 2: "/files?device=hf&page=2"} {
		if got := p.URL(n); got != want {
			t.Errorf("URL(%d) = %q, want %q", n, got, want)
		}
	}

	if got := (layout.Paging{Path: "/decodes"}).URL(1); got != "/decodes" {
		t.Errorf("first page without filters = %q", got)
	}
}
