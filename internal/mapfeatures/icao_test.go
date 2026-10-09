package mapfeatures

import "testing"

// The allocation table: blocks sorted and disjoint, aligned on their size.
func TestICAOBlocks(t *testing.T) {
	for i, b := range icaoBlocks {
		size := b.hi - b.lo + 1
		if b.hi < b.lo || size&(size-1) != 0 || b.lo%size != 0 {
			t.Errorf("block %06X-%06X: not an aligned power of two", b.lo, b.hi)
		}

		if i > 0 && icaoBlocks[i-1].hi >= b.lo {
			t.Errorf("block %06X overlaps %06X", b.lo, icaoBlocks[i-1].lo)
		}
	}
}

func TestICAOCountry(t *testing.T) {
	cases := []struct {
		addr      uint32
		iso, name string
		ok        bool
	}{
		{0x3C65A1, "DE", "Germany", true},
		{0x4CA1FA, "IE", "Ireland", true},
		{0xA835AF, "US", "United States", true},
		{0x7C4920, "AU", "Australia", true},
		{0x501C00, "HR", "Croatia", true},
		{0x501BFF, "", "", false},
		{0x004000, "ZW", "Zimbabwe", true},
		{0xF00010, "", "ICAO (temporary)", true},
		{0x000000, "", "", false},
		{0xFFFFFF, "", "", false},
	}

	for _, c := range cases {
		iso, name, ok := ICAOCountry(c.addr)
		if iso != c.iso || name != c.name || ok != c.ok {
			t.Errorf("%06X: %q %q %v, want %q %q %v", c.addr, iso, name, ok, c.iso, c.name, c.ok)
		}
	}
}

// Registrations of the regular allocations; the first and last of each
// block, and aircraft whose address is known (N628TS, D-AIMA).
func TestICAORegistration(t *testing.T) {
	cases := map[uint32]string{
		// United States.
		0xA00001: "N1", 0xA00002: "N1A", 0xA00003: "N1AA", 0xA0001A: "N1AZ", 0xA0025A: "N10", 0xA061D9: "N12345",
		0xA05460: "N120GZ", 0xA835AF: "N628TS", 0xADF7C7: "N99999", 0xA00000: "", 0xADF7C8: "",
		// Japan, South Korea.
		0x840000: "JA0000", 0x840308: "JA00UE", 0x86CF59: "JA8089", 0x874BCF: "JA99ZZ",
		0x71BA00: "HL7200", 0x71BE13: "HL7613", 0x71C299: "HL8299", 0x71BA0A: "",
		// Numeric.
		0x140000: "RA-00000", 0x15869F: "RA-99999", 0x0B03E8: "CU-T1000", 0x0B07CF: "CU-T1999",
		// Three-letter suffixes.
		0x3C0001: "D-APAA", 0x3C65A1: "D-AIMA", 0x3C7F5A: "D-AOZZ", 0x3CC000: "D-CAAA", 0x3E5BEF: "D-IZZZ",
		0x390000: "F-GAAA", 0x3949E0: "F-GSPA", 0x39E739: "F-HZZZ", 0xC00001: "C-FAAA", 0xC03326: "C-FTJP", 0xC08950: "C-GZZZ",
		0x7C0000: "VH-AAA", 0x7C4920: "VH-OQA", 0x7C822D: "VH-ZZZ", 0x008011: "ZS-AAA", 0x448897: "OO-BDW", 0xE1A69A: "LV-ZZZ",
		0x768421: "9V-AAA",
		// A stride gap and an address of a state without a pattern.
		0x39001A: "", 0x400123: "",
	}

	for addr, want := range cases {
		if got := ICAORegistration(addr); got != want {
			t.Errorf("%06X: %q, want %q", addr, got, want)
		}
	}
}

func TestParseICAO(t *testing.T) {
	for in, want := range map[string]bool{"4CA1FA": true, "4ca1fa": true, "~4CA1FA": false, "4CA1F": false, "4CA1FG": false, "+CA1FA": false} {
		if _, ok := ParseICAO(in); ok != want {
			t.Errorf("%q: %v", in, ok)
		}
	}
}
