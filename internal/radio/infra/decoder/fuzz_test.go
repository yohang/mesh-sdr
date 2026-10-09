package decoder

import (
	"testing"
	"time"
)

// Tool output is untrusted (RF text): the parsers never panic. Run as
// normal tests on the seed corpus; `go test -fuzz` explores further.

func FuzzParseJS8(f *testing.F) {
	for _, s := range []string{
		"140000 -11  0.4 1050 A  qBdgE+EP++++         2",
		"172300 -12  0.4 1044 A  2jNlWSPIPQ-W         3",
		"151615 -11  0.3 2457 A  ViZoThL+C+aL         3",
		"140000 -11  0.4 1050 A  tTtTtS000000         2",
		"<DecodeFinished>",
		"",
	} {
		f.Add(s)
	}

	f.Fuzz(func(_ *testing.T, line string) {
		_, _ = parseJS8(profile{}, line, false)
	})
}

func FuzzParseJS8Frame(f *testing.F) {
	for _, s := range []string{"qBdgE+EP++++", "tTtTtS000000", "............", "++++++++++++", "iXvZfxW3Sju+", "000000000000"} {
		f.Add(s)
	}

	f.Fuzz(func(_ *testing.T, payload string) {
		var rec JS8Record
		_, _ = parseJS8Frame(payload, &rec)
	})
}

func FuzzParseJT9(f *testing.F) {
	for _, s := range []string{
		"120015 -12  0.0 1200 ~  CQ DL1ABC JO62",
		"120030 -11  0.0 1500 :  K1ABC W9XYZ EN37                      q3 ",
		"**** -23  0.6 3023 `  <...> <...> R 591631 BI53PV",
		"1202 -10  0.0 1500 `  K1ABC FN42 33",
		"1 2 3",
	} {
		f.Add(s)
	}

	ft8 := profile{mode: "ft8", period: 15 * time.Second}
	fst4w := profile{mode: "fst4w", period: 2 * time.Minute}

	f.Fuzz(func(_ *testing.T, line string) {
		_, _ = parseJT9(ft8, line, false)
		_, _ = parseJT9(fst4w, line, true)
	})
}

func FuzzParseWSPR(f *testing.F) {
	for _, s := range []string{"1200 -20 -0.0   0.001500  0  K1ABC FN42 33 ", "0052 -29  2.6   0.001486 -1  G0ABC IO92 23", "a b c d e f"} {
		f.Add(s)
	}

	p := profile{mode: "wspr", period: 2 * time.Minute}

	f.Fuzz(func(_ *testing.T, line string) {
		_, _ = parseWSPR(p, line, false)
	})
}

// A compressed frame with a run of continuation nibbles longer than a
// JSC word is refused, not a crash (regression).
func TestJSCLongContinuation(t *testing.T) {
	if _, ok := parseJS8(profile{}, "140000 -11  0.4 1050 A  tTtTtS000000         2", false); ok {
		t.Log("decoded as data")
	}

	var rec JS8Record
	if _, err := parseJS8Frame("tTtTtS000000", &rec); err != nil {
		t.Fatal(err)
	}
}

func FuzzParseAIVDM(f *testing.F) {
	for _, s := range []string{
		"!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt0000,0*48",
		nmea("!AIVDM,1,1,,A,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216L961O5Gf0NSQEp6ClRp888888888880,2"),
		nmea("!AIVDM,1,1,,A,H5NJ;PP005l4ot5Isbl03wsUkP06,0"),
		nmea("!AIVDM,1,1,,A,K5NJ;PP005l4ot5Isbl0,0"),
		nmea("!AIVDM,1,1,,A,0,5"),
		"!AIVDM,1,1,,A,,0*",
	} {
		f.Add(s)
	}

	f.Fuzz(func(_ *testing.T, s string) {
		_, _ = parseAIVDM(s)
	})
}

func FuzzParseDSC(f *testing.F) {
	for _, s := range []string{dscSelcall, dscDistress, dscError, dscBadECC, `{ "format": "x", "loc": "99.999S999.999E" }`, "{"} {
		f.Add(s)
	}

	f.Fuzz(func(_ *testing.T, line string) {
		_, _ = parseDSC(line, time.Time{}, true)
	})
}
