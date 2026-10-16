package decoder

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// libcsdr++ 0.18.41 DscDecoder lines, written by the native DSC chain from
// synthesised calls (dsptest.DSC).
const (
	dscSelcall  = `{ "format": "selcall", "src": "227006760", "dst": "002275300", "rxfreq": "8414000", "txfreq": "8414000", "category": "routine", "cmd1": 100, "cmd2": 126, "eos": "arq", "ecc": true, "timestamp": 1791549788 }`
	dscDistress = `{ "format": "distress", "src": "227006760", "loc": "48.500N4.250W", "time": "1234", "distress": "disabled / adrift", "next": 100, "eos": "done", "ecc": true, "timestamp": 1791549788 }`
	dscError    = `{ "format": "error", "data": "111 110 109 108 107 106 105 104 120 120 1 2 3 4 5|6 7 8 9 10", "timestamp": 1791549788 }`
	dscBadECC   = `{ "format": "selcall", "src": "2270067--", "dst": "002275300", "rxfreq": "CH16", "category": "routine", "cmd1": 100, "cmd2": 126, "eos": "arq", "ecc": false, "timestamp": 1791549789 }`
)

func TestParseDSC(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	for _, tc := range []struct {
		name, line string
		errors     bool
		text       string
		want       *DSCRecord
	}{
		{
			name: "selcall", line: dscSelcall,
			text: "selcall routine from 227006760 (France) to 002275300 rx 8414.0 kHz tx 8414.0 kHz arq",
			want: &DSCRecord{
				Format: "selcall", Source: "227006760", SourceKind: MMSIShip, Country: "France", Destination: "002275300", RxFreq: "8414000",
				TxFreq: "8414000", Category: "routine", Cmd1: ptr(100), Cmd2: ptr(126), EOS: "arq", ECC: ptr(true),
			},
		},
		{
			name: "distress with position", line: dscDistress,
			text: "distress from 227006760 (France) disabled / adrift at 48.500N4.250W 1234 UTC done",
			want: &DSCRecord{
				Format: "distress", Source: "227006760", SourceKind: MMSIShip, Country: "France", Location: "48.500N4.250W", Lat: ptr(48.5),
				Lon: ptr(-4.25), Time: "1234", Distress: "disabled / adrift", Next: ptr(100), EOS: "done", ECC: ptr(true),
			},
		},
		{
			name: "bad ecc, digits in error", line: dscBadECC,
			text: "selcall routine from 2270067-- to 002275300 rx CH16 arq ECC error",
			want: &DSCRecord{
				Format: "selcall", Source: "2270067--", Destination: "002275300", RxFreq: "CH16", Category: "routine", Cmd1: ptr(100), Cmd2: ptr(126),
				EOS: "arq", ECC: ptr(false),
			},
		},
		{name: "error hidden", line: dscError},
		{
			name: "error shown", line: dscError, errors: true,
			text: "error 111 110 109 108 107 106 105 104 120 120 1 2 3 4 5|6 7 8 9 10",
			want: &DSCRecord{Format: "error", Data: "111 110 109 108 107 106 105 104 120 120 1 2 3 4 5|6 7 8 9 10"},
		},
		{name: "not json", line: `{ "format": "sel`},
		{name: "no format", line: `{ "src": "227006760" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := parseDSC(tc.line, at, tc.errors)
			if ok != (tc.want != nil) {
				t.Fatalf("ok %v", ok)
			}

			if !ok {
				return
			}

			var got DSCRecord
			if err := json.Unmarshal(rec.Payload, &got); err != nil {
				t.Fatal(err)
			}

			if rec.Text != tc.text || rec.Schema != DSCSchema || !rec.Time.Equal(at) || !reflect.DeepEqual(&got, tc.want) {
				t.Fatalf("record %q %s %+v", rec.Text, rec.Payload, got)
			}
		})
	}
}

func TestMMSIInfo(t *testing.T) {
	for _, tc := range []struct {
		mmsi string
		want MMSIInfo
		ok   bool
	}{
		{"227006760", MMSIInfo{Kind: MMSIShip, MID: 227, Country: "France"}, true},
		{"366123456", MMSIInfo{Kind: MMSIShip, MID: 366, Country: "United States"}, true},
		{"002275300", MMSIInfo{Kind: MMSICoast, MID: 227, Country: "France"}, true},
		{"023200000", MMSIInfo{Kind: MMSIGroup, MID: 232, Country: "United Kingdom"}, true},
		{"111232501", MMSIInfo{Kind: MMSIAircraft, MID: 232, Country: "United Kingdom"}, true},
		{"992276001", MMSIInfo{Kind: MMSIAtoN, MID: 227, Country: "France"}, true},
		{"982270001", MMSIInfo{Kind: MMSIAuxCraft, MID: 227, Country: "France"}, true},
		{"822701234", MMSIInfo{Kind: MMSIHandheld, MID: 227, Country: "France"}, true},
		{"970123456", MMSIInfo{Kind: MMSISART}, true},
		{"972123456", MMSIInfo{Kind: MMSIMOB}, true},
		{"974123456", MMSIInfo{Kind: MMSIEPIRB}, true},
		{"299123456", MMSIInfo{Kind: MMSIShip}, true},
		{"2270067--", MMSIInfo{}, false},
		{"22700676", MMSIInfo{}, false},
	} {
		if got, ok := mmsiInfo(tc.mmsi); got != tc.want || ok != tc.ok {
			t.Errorf("%s: %+v %v", tc.mmsi, got, ok)
		}
	}
}

// TestNAVTEXTagger: the lines of a message carry its header, until NNNN.
func TestNAVTEXTagger(t *testing.T) {
	var got []app.DecodeRecord

	l := &lineAssembler{emit: (&navtexTagger{emit: func(r app.DecodeRecord) { got = append(got, r) }}).record, now: time.Now}
	l.feed([]byte("ZCZC EA01\r\nGALE WARNING\r\nNNNN\r\n\nZCZC FZ99\r\n"), time.Unix(1_800_000_000, 0))

	want := []NAVTEXRecord{
		{Text: "ZCZC EA01", Station: "E", Subject: "A", SubjectLabel: "Navigational warning", Serial: "01"},
		{Text: "GALE WARNING", Station: "E", Subject: "A", SubjectLabel: "Navigational warning", Serial: "01"},
		{Text: "NNNN", Station: "E", Subject: "A", SubjectLabel: "Navigational warning", Serial: "01"},
		{Text: "ZCZC FZ99", Station: "F", Subject: "Z", SubjectLabel: "No message on hand", Serial: "99"},
	}

	if len(got) != len(want) {
		t.Fatalf("records %+v", got)
	}

	for i, r := range got {
		var p NAVTEXRecord
		if err := json.Unmarshal(r.Payload, &p); err != nil || r.Schema != NAVTEXSchema || p != want[i] || r.Text != want[i].Text {
			t.Errorf("record %d: %+v %+v %v", i, r, p, err)
		}
	}

	// Without NNNN, the next lines keep the header of their message.
	got = nil
	l.feed([]byte("NOISE\r\n"), time.Unix(1_800_000_000, 0))

	var p NAVTEXRecord
	if err := json.Unmarshal(got[0].Payload, &p); err != nil || p != (NAVTEXRecord{Text: "NOISE", Station: "F", Subject: "Z", SubjectLabel: "No message on hand", Serial: "99"}) {
		t.Fatalf("%+v %v", p, err)
	}
}

// TestDSCLines: one record per JSON line, whatever the blocks.
func TestDSCLines(t *testing.T) {
	var got []app.DecodeRecord

	d := &dscLines{emit: func(r app.DecodeRecord) { got = append(got, r) }}
	at := time.Unix(1_800_000_000, 0)
	all := dscSelcall + "\n" + dscError + "\n" + strings.Repeat("x", maxDSCLine+10) + "\n" + dscDistress

	for i := 0; i < len(all); i += 37 {
		d.feed([]byte(all[i:min(i+37, len(all))]), at.Add(time.Duration(i)*time.Millisecond))
	}

	if len(got) != 1 || !strings.HasPrefix(got[0].Text, "selcall") || !got[0].Time.Equal(at) {
		t.Fatalf("records %+v", got)
	}

	// The line being written is dropped on lost input.
	d.flush()
	d.feed([]byte("\n"), at)

	if len(got) != 1 {
		t.Fatalf("records %+v", got)
	}
}

// TestMarineSessions decodes NAVTEX and DSC from the selector IQ of a
// demodulator, through the runner.
func TestMarineSessions(t *testing.T) {
	const rate = 24000.0

	for _, tc := range []struct {
		mode   string
		iq     []complex64
		schema string
		text   string
	}{
		{
			mode: "navtex", iq: dsptest.SITORB("RYRY\r\nZCZC EA01\r\nGALE WARNING\r\nNNNN\r\n\nZCZC EZ02\r\nQRU\r\nNNNN\r\n\n", 170, rate, 1500),
			schema: NAVTEXSchema, text: "GALE WARNING",
		},
		{
			mode: "dsc", iq: dsptest.DSC(dscTestCall(), 170, rate, 1500),
			schema: DSCSchema, text: "selcall routine from 227006760 (France) to 002275300 rx 8414.0 kHz tx 8414.0 kHz arq",
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := NewRunner(Options{})
			ev := &textEvents{}

			run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("01890a5d-ac96-7a3b-8000-000000000009"), Mode: textMode(t, tc.mode), OffsetHz: 1500}, ev.events())
			if err != nil {
				t.Fatal(err)
			}
			defer run.Close()

			s := run.(*textSession)

			for i := 0; i < len(tc.iq); i += 2400 {
				run.IQ(app.IQBlock{Samples: tc.iq[i:min(i+2400, len(tc.iq))], Rate: rate, Time: time.Unix(1_800_000_000, 0)})
				ev.wait(t, "the input taken", func() bool { return s.in.len() == 0 })
			}

			ev.wait(t, tc.text, func() bool {
				for _, l := range ev.lines {
					if l.Text == tc.text && l.Schema == tc.schema {
						return true
					}
				}

				return false
			})
		})
	}
}

// dscTestCall is the call of the native chain test (internal/dsp): a
// routine individual call from 227006760 to 002275300 on 8414.0 kHz.
func dscTestCall() []int {
	msg := []int{120, 0, 22, 75, 30, 0, 100, 22, 70, 6, 76, 0, 100, 126, 8, 41, 40, 8, 41, 40, 117}
	ecc := 0

	for _, c := range msg {
		ecc ^= c
	}

	return append(append([]int{120}, msg...), ecc, 117, 117)
}
