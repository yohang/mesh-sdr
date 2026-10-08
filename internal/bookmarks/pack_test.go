package bookmarks

import (
	"slices"
	"testing"
)

// TestLoadPacks parses the embedded OpenWebRX+ packs: every file, merged
// by unique key, with region tags and stable UUIDv5 ids.
func TestLoadPacks(t *testing.T) {
	p, err := LoadPacks()
	if err != nil {
		t.Fatal(err)
	}

	want := []PackFile{
		{"aviation", "", 166}, {"cb", "", 40}, {"marine", "", 24}, {"misc", "", 22}, {"vhf-marine", "", 102}, {"wfax", "", 88},
		{"lpd", "r1", 69}, {"pmr", "r1", 16}, {"aar", "r2", 97}, {"gmrs", "r2", 30}, {"murs", "r2", 5}, {"pmr", "r3", 16},
	}
	if got := p.Files(); !slices.Equal(got, want) {
		t.Fatalf("files = %+v, want %+v", got, want)
	}

	// The PMR channels of R1 and R3 are the same bookmarks: one row each,
	// tagged with both regions.
	if p.Len() != 675-16 {
		t.Fatalf("%d distinct pack bookmarks, want %d", p.Len(), 675-16)
	}

	byName := map[string]*packEntry{}
	for _, e := range p.entries {
		if _, ok := byName[e.name+"@"+e.modulation]; !ok {
			byName[e.name+"@"+e.modulation] = e // the lowest frequency
		}
	}

	pmr := byName["PMR1@nfm"]
	if pmr == nil || !slices.Equal(pmr.tags(), []string{"pack:pmr", "region:r1", "region:r3"}) || !pmr.scannable {
		t.Fatalf("PMR1 = %+v", pmr)
	}

	if e := byName["LPD1@nfm"]; e == nil || !slices.Equal(e.tags(), []string{"pack:lpd", "region:r1"}) {
		t.Errorf("LPD1 = %+v", e)
	}

	if e := byName["Emergency@am"]; e == nil || !slices.Equal(e.tags(), []string{"pack:aviation"}) || e.frequency != 121_500_000 {
		t.Errorf("Emergency = %+v", e)
	}

	// Digital modes are kept as is, with their underlying mode, and are
	// not scannable (OpenWebRX+ rule).
	var rtty *packEntry

	for _, e := range p.entries {
		if e.modulation == "rtty450" {
			rtty = e

			break
		}
	}

	if rtty == nil || rtty.underlying != "usb" || rtty.scannable {
		t.Errorf("rtty450 entry = %+v", rtty)
	}

	// Sorted by frequency; ids are UUIDv5, stable and distinct.
	ids := map[string]bool{}

	for i, e := range p.entries {
		if i > 0 && e.frequency < p.entries[i-1].frequency {
			t.Fatalf("entries not sorted at %d", i)
		}

		id := e.id()
		if id.Version() != 5 || ids[id.String()] || e.id() != id {
			t.Fatalf("id of %q = %s", e.name, id)
		}

		ids[id.String()] = true
	}
}

func TestModeMapping(t *testing.T) {
	for in, want := range map[string]string{"NFM": "nfm", " usb ": "usb", "wfm": "wfm", "hfdl": "hfdl", "": ""} {
		if got := modeOf(in); got != want {
			t.Errorf("modeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBandplans parses the band plans of the three regions.
func TestBandplans(t *testing.T) {
	plans, err := LoadBandplans()
	if err != nil {
		t.Fatal(err)
	}

	for region, n := range map[string]int{"r1": 45, "r2": 46, "r3": 43} {
		p := plans[region]
		if p == nil || p.Region() != region || len(p.Bands(0, 1<<62)) != n {
			t.Errorf("%s: %d bands, want %d", region, len(p.Bands(0, 1<<62)), n)
		}
	}

	r1 := plans["r1"]

	bands := r1.Bands(3_600_000, 3_600_000)
	if len(bands) != 1 || bands[0].Name != "80m" || bands[0].Low != 3_500_000 || !slices.Equal(bands[0].Tags, []string{"hamradio"}) {
		t.Fatalf("bands at 3.6 MHz = %+v", bands)
	}

	dials := r1.Dials(3_500_000, 3_800_000)
	if len(dials) == 0 || !slices.IsSortedFunc(dials, func(a, b Dial) int { return int(a.Frequency - b.Frequency) }) {
		t.Fatalf("80m dials = %+v", dials)
	}

	var ft4, sstv int

	for _, d := range dials {
		switch {
		case d.Mode == "ft4":
			ft4++
		case d.Mode == "sstv" && d.Underlying == "lsb" && d.Frequency == 3_730_000 && d.Band == "80m":
			sstv++
		}
	}

	if ft4 != 2 || sstv != 1 {
		t.Errorf("80m: %d ft4 and %d sstv dials: %+v", ft4, sstv, dials)
	}

	if got := r1.Dials(1, 2); len(got) != 0 {
		t.Errorf("dials out of every band = %+v", got)
	}
}
