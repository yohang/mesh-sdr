package decoder

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

func pagingPayload(t *testing.T, rec app.DecodeRecord) PagingRecord {
	t.Helper()

	var p PagingRecord
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatal(err)
	}

	return p
}

// multimon-ng 1.3.1 lines (pocsag.c, demod_flex.c formats).
func TestPagingParser(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	for _, tc := range []struct {
		name   string
		filter bool
		lines  []string
		texts  []string
	}{
		{
			name: "pocsag alpha, numeric, tone",
			lines: []string{
				"POCSAG1200: Address: 1234567  Function: 3  Alpha:   Hello MeshSDR<EOT>",
				"POCSAG512: Address:   12345  Function: 0  Numeric: 0612 345",
				"POCSAG2400: Address:  200000  Function: 1 ",
				"POCSAG1200: Address: 1234567  Function: 3  Certainty:     0  Alpha:   x<NUL><NUL>",
				"multimon-ng 1.3.1",
			},
			texts: []string{
				"POCSAG1200 1234567/3 Alpha: Hello MeshSDR", "POCSAG512 12345/0 Numeric: 0612 345", "POCSAG2400 200000/1", "POCSAG1200 1234567/3 Alpha: x",
			},
		},
		{
			name: "filtered: readable alphanumeric only", filter: true,
			lines: []string{
				"POCSAG1200: Address: 1234567  Function: 3  Alpha:   Fire at station 2",
				"POCSAG512: Address:   12345  Function: 0  Numeric: 0612",
				"POCSAG1200: Address: 1234567  Function: 3  Alpha:   <NUL>",
				"FLEX|2026-10-08 12:00:00|1600/2/K/A|07.104|001234567|ALN|Meet at gate 4",
				"FLEX|2026-10-08 12:00:00|1600/2/K/A|07.104|001234567|ALN|" + strings.Repeat("x", 120),
				"FLEX: 2026-10-08 12:00:00 1600/2/A 07.104 [001234567] NUM 0123",
			},
			texts: []string{"POCSAG1200 1234567/3 Alpha: Fire at station 2", "FLEX1600 001234567 ALN: Meet at gate 4"},
		},
		{
			name: "flex fragments joined per capcode",
			lines: []string{
				"FLEX|2026-10-08 12:00:00|1600/2/F/A|07.104|001234567|ALN|The first part ",
				"FLEX|2026-10-08 12:00:00|1600/2/F/A|07.104|000000042|ALN|Other ",
				"FLEX|2026-10-08 12:00:00|1600/2/F/A|07.105|001234567|ALN|and the second ",
				"FLEX|2026-10-08 12:00:00|1600/2/C/A|07.106|001234567|ALN|and the last.",
				"FLEX|2026-10-08 12:00:00|1600/2/C/A|07.106|000000042|ALN|page",
				"FLEX|2026-10-08 12:00:00|3200/4/K/B|07.107|000000001 000000002|ALN|Group",
				"FLEX: 2026-10-08 12:00:00 1600/2/A 07.104 [001234567] TON ",
			},
			texts: []string{
				"FLEX1600 001234567 ALN: The first part and the second and the last.", "FLEX1600 000000042 ALN: Other page",
				"FLEX3200 000000001 000000002 ALN: Group", "FLEX1600 001234567 TON",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parse := newPagingParser(sessionConfig{settings: Settings{PagingFilter: tc.filter}})

			var texts []string

			for _, l := range tc.lines {
				for _, rec := range parse(l, at) {
					if rec.Schema != PagingSchema || !rec.Time.Equal(at) {
						t.Errorf("record %+v", rec)
					}

					texts = append(texts, rec.Text)
				}
			}

			if !slices.Equal(texts, tc.texts) {
				t.Errorf("texts:\n%q\nwant\n%q", texts, tc.texts)
			}
		})
	}

	rec := newPagingParser(sessionConfig{})("POCSAG1200: Address: 1234567  Function: 3  Alpha:   Hi", at)[0]
	if p := pagingPayload(t, rec); !reflect.DeepEqual(p, PagingRecord{Protocol: "POCSAG", Baud: 1200, Address: "1234567", Function: ptr(3), Type: "Alpha", Message: "Hi"}) {
		t.Errorf("payload = %+v", p)
	}
}

// The FLEX buffer is capped: per capcode and in capcodes.
func TestPagingFlexCap(t *testing.T) {
	p := &pagingParser{flex: map[string]string{}}

	for i := range maxFlexPending + 10 {
		p.assemble(strconv.Itoa(i), "F", "x")
	}

	if len(p.flex) != maxFlexPending || len(p.order) != maxFlexPending {
		t.Errorf("pending = %d, %d", len(p.flex), len(p.order))
	}

	if _, ok := p.flex["0"]; ok {
		t.Error("the oldest capcode was kept")
	}

	for range 3 {
		p.assemble("big", "F", strings.Repeat("y", 2000))
	}

	msg, ok := p.assemble("big", "C", "end")
	if !ok || len(msg) != maxFlexBytes {
		t.Errorf("message of %d bytes, complete %v", len(msg), ok)
	}
}

func TestPagingArgs(t *testing.T) {
	for cs, want := range map[string]string{"": "US", "DE": "DE", "XX": "US"} {
		args := pagingArgs(sessionConfig{settings: Settings{PagingCharset: cs}})
		if i := slices.Index(args, "-C"); i < 0 || args[i+1] != want {
			t.Errorf("%q: %q", cs, args)
		}
	}
}

func TestParseEAS(t *testing.T) {
	at := time.Date(2026, 10, 8, 17, 1, 0, 0, time.UTC)

	rec, ok := parseEAS("EAS: ZCZC-WXR-TOR-029095-029037-020000+0030-2811700-KEAX/NWS-", at)
	if !ok {
		t.Fatal("not parsed")
	}

	var p EASRecord
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatal(err)
	}

	start, end := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 17, 30, 0, 0, time.UTC)
	want := EASRecord{
		Raw: "ZCZC-WXR-TOR-029095-029037-020000+0030-2811700-KEAX/NWS-", Originator: "WXR", OrigName: "National Weather Service",
		Event: "TOR", EventName: "Tornado Warning", Purge: "0030", Start: &start, End: &end, Station: "KEAX/NWS",
		Areas: []EASArea{{Code: "029095", Name: "Jackson, Missouri"}, {Code: "029037", Name: "Cass, Missouri"}, {Code: "020000", Name: "All of Kansas"}},
	}

	if !reflect.DeepEqual(p, want) {
		t.Errorf("payload = %+v", p)
	}

	wantText := "ZCZC-WXR-TOR-029095-029037-020000+0030-2811700-KEAX/NWS-\nTornado Warning from National Weather Service for Jackson, Missouri; " +
		"Cass, Missouri; All of Kansas, 2026-10-08 17:00 to 2026-10-08 17:30 UTC (KEAX/NWS)"
	if rec.Text != wantText || rec.Schema != EASSchema {
		t.Errorf("text = %q", rec.Text)
	}

	// The day of year resolves to the year nearest to the reception.
	if rec, ok := parseEAS("EAS: ZCZC-CIV-ZZW-099999+0100-3652330-ABCD    -", time.Date(2027, 1, 1, 0, 10, 0, 0, time.UTC)); !ok {
		t.Error("unknown codes refused")
	} else if !strings.Contains(rec.Text, "Unknown Warning") || !strings.Contains(rec.Text, "099999") || !strings.Contains(rec.Text, "2026-12-31 23:30") {
		t.Errorf("text = %q", rec.Text)
	}

	for _, bad := range []string{"EAS: NNNN", "EAS: ZCZC-WXR", "ZCZC-WXR-TOR-029095+0030-2811700-KEAX/NWS-", "EAS: ZCZC-WXR-TOR-029095+0030-2811700-<script>-"} {
		if _, ok := parseEAS(bad, at); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCallsignCountry(t *testing.T) {
	for call, want := range map[string]string{
		"F4ABC": "FR", "DL1ABC": "DE", "K1ABC": "US", "G4XYZ": "GB", "VK2AB": "AU", "JA1XYZ": "JP",
		"CQ": "", "TEST": "", "F4": "", "1234": "", "DE": "",
	} {
		c, ok := callsignCountry(call)
		if ok != (want != "") || c[0] != want {
			t.Errorf("%s = %v %v, want %q", call, c, ok, want)
		}
	}
}
