package decoder

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

func TestSkimmerParser(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	dial := int64(14_000_000)
	parse := newSkimmerParser("CW")(sessionConfig{dial: func() int64 { return dial }})

	var spots []SkimmerRecord

	texts := 0

	for _, l := range []string{"1000:29:CQ C", "1000:27:Q CQ ", "900:19:Q CQ ", "1000:41:DE F", "1000:35:4ABC ", "1000:47:F4AB", "garbage", "2000:12:TU F4"} {
		for _, rec := range parse(l, at) {
			var p SkimmerRecord
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				t.Fatal(err)
			}

			if p.Kind == "text" {
				texts++

				if !rec.Live || rec.AudioHz != p.OffsetHz {
					t.Errorf("text record %+v", rec)
				}

				continue
			}

			if rec.Live || rec.AudioHz != 1000 || rec.Text != "F4ABC (France): CQ CQ CQ DE F4ABC" {
				t.Errorf("spot record %+v", rec)
			}

			spots = append(spots, p)
		}
	}

	want := SkimmerRecord{Kind: "spot", Mode: "CW", OffsetHz: 1000, DB: 35, Callsign: "F4ABC", CountryCode: "FR", Country: "France", Message: "CQ CQ CQ DE F4ABC"}
	if texts != 7 || len(spots) != 1 || !reflect.DeepEqual(spots[0], want) {
		t.Errorf("texts %d, spots %+v", texts, spots)
	}

	// "<callee> DE <call>", "<call> <call>", a dial change.
	dial = 7_100_000
	rtty := newSkimmerParser("RTTY")(sessionConfig{dial: func() int64 { return dial }})

	for _, tc := range []struct{ line, call, callee string }{
		{"1500:10: DL1ABC DE G4XYZ K", "G4XYZ", "DL1ABC"},
		{"1600:10: EA4AA EA4AA ", "EA4AA", ""},
	} {
		recs := rtty(tc.line, at)
		if len(recs) != 2 {
			t.Fatalf("%s: %d records", tc.line, len(recs))
		}

		var p SkimmerRecord
		_ = json.Unmarshal(recs[1].Payload, &p)

		if p.Callsign != tc.call || p.Callee != tc.callee || p.Mode != "RTTY" {
			t.Errorf("%s: %+v", tc.line, p)
		}
	}

	dial = 7_200_000

	var p SkimmerRecord
	_ = json.Unmarshal(rtty("1500:10:ABC", at)[0].Payload, &p)

	if !p.Changed {
		t.Error("no changed flag after a dial change")
	}
}

// The text log joins the characters of each frequency into lines (line
// break, maxLine characters, lineIdle without a character) and is handed
// over when full, when it covers an hour, or when the session ends; never
// empty.
func TestTextLog(t *testing.T) {
	var l textLog

	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	rec := func(at time.Duration, hz int64, text string) app.DecodeRecord {
		return app.DecodeRecord{Time: t0.Add(at), Text: text, AudioHz: hz, Live: true}
	}

	if f := l.flush(t0); f != nil {
		t.Errorf("empty log saved: %+v", f)
	}

	// Two frequencies interleaved, a line break on one.
	for _, r := range []app.DecodeRecord{
		rec(0, 1000, "CQ C"), rec(time.Second, 1500, "TEST"), rec(2*time.Second, 1000, "Q DE F4ABC"), rec(3*time.Second, 1500, " K\n"),
	} {
		if f := l.add(r, 14_000_000, r.Time); f != nil {
			t.Fatalf("saved early: %q", f.Data)
		}
	}

	// Idle: the 1000 Hz line ends after lineIdle.
	if f := l.tick(t0.Add(3 * time.Second)); f != nil {
		t.Fatalf("saved early: %q", f.Data)
	}

	l.tick(t0.Add(8 * time.Second))

	// 90 characters: a line of maxLine, then the rest.
	l.add(rec(10*time.Second, 2000, strings.Repeat("x", 90)), 14_000_000, t0.Add(10*time.Second))

	f := l.flush(t0.Add(11 * time.Second))
	if f == nil {
		t.Fatal("nothing saved")
	}

	want := "2026-10-08T12:00:01Z 14001500 TEST K\n2026-10-08T12:00:00Z 14001000 CQ CQ DE F4ABC\n" +
		"2026-10-08T12:00:10Z 14002000 " + strings.Repeat("x", 80) + "\n2026-10-08T12:00:10Z 14002000 xxxxxxxxxx\n"
	if string(f.Data) != want || f.Kind != app.FileTextLog || f.FreqHz != 14_000_000 || !f.Start.Equal(t0.Add(time.Second)) ||
		!f.End.Equal(t0.Add(11*time.Second)) {
		t.Errorf("log %+v\n%q", f, f.Data)
	}

	if f := l.flush(t0.Add(time.Minute)); f != nil {
		t.Errorf("flushed twice: %+v", f)
	}

	// An hour of lines is saved on the next tick; a log without a dial
	// is never started.
	l.add(rec(0, 0, "A\n"), 7_000_000, t0)

	if f := l.add(rec(time.Hour, 0, "B\n"), 7_000_000, t0.Add(time.Hour)); f == nil || string(f.Data) != "2026-10-08T12:00:00Z 7000000 A\n2026-10-08T13:00:00Z 7000000 B\n" {
		t.Errorf("hour log %+v", f)
	}

	if f := l.add(rec(0, 0, "C\n"), 0, t0); f != nil || l.flush(t0) != nil {
		t.Error("log without a dial")
	}

	// The size cap.
	var big textLog

	n := 0

	for i := range 30_000 {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		if f := big.add(app.DecodeRecord{Time: at, Text: strings.Repeat("x", 40) + "\n"}, 1, at); f != nil {
			n++

			if len(f.Data) < textLogBytes {
				t.Errorf("log of %d bytes", len(f.Data))
			}
		}
	}

	if n != 1 {
		t.Errorf("%d full logs", n)
	}
}

// rtl_433 25.02 JSON lines (-F json -M time:unix -M level).
func TestParseISM(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	line := `{"time" : "1800000000", "model" : "Nexus-TH", "id" : 177, "channel" : 1, "battery_ok" : 1, "temperature_C" : 21.5, ` +
		`"humidity" : 45, "mod" : "ASK", "freq" : 433.92, "rssi" : -0.105, "snr" : 22.1, "noise" : -22.2}`

	rec, ok := parseISM(line, "ISM", false, at)
	if !ok {
		t.Fatal("not parsed")
	}

	wantPayload := `{"mode":"ISM","time":"1800000000","model":"Nexus-TH","id":177,"channel":1,"battery_ok":1,"temperature_C":21.5,"humidity":45,"mod":"ASK","freq":433.92}`
	if string(rec.Payload) != wantPayload || rec.Text != "Nexus-TH id=177 channel=1 battery_ok=1 temperature_C=21.5 humidity=45" || rec.Schema != ISMSchema {
		t.Errorf("record %q %q", rec.Payload, rec.Text)
	}

	rec, _ = parseISM(line, "WMBUS", true, at)
	if !strings.HasPrefix(string(rec.Payload), `{"mode":"WMBUS"`) || !strings.Contains(string(rec.Payload), `"rssi":-0.105,"snr":22.1,"noise":-22.2}`) {
		t.Errorf("payload with levels %s", rec.Payload)
	}

	if !json.Valid(rec.Payload) {
		t.Error("invalid payload")
	}

	// RF strings are cleaned, nested ones too.
	rec, _ = parseISM(`{"model" : "X\u0007", "code" : "a\u001bb", "rows" : [{"data" : "c\u0000d"}]}`, "ISM", false, at)
	if string(rec.Payload) != `{"mode":"ISM","model":"X","code":"ab","rows":[{"data":"cd"}]}` || rec.Text != `X code=ab rows=[{"data":"cd"}]` {
		t.Errorf("cleaned %s %q", rec.Payload, rec.Text)
	}

	for _, bad := range []string{
		`{"time" : "1800000000", "frames" : {"count" : 0}, "stats" : []}`,
		`rtl_433 version 25.02`,
		`{"model" : "x"`,
		`{"model" : ""}`,
		`{"model" : 5}`,
	} {
		if _, ok := parseISM(bad, "ISM", true, at); ok {
			t.Errorf("%.40q parsed", bad)
		}
	}
}

func TestRTL433Args(t *testing.T) {
	ism := strings.Join(ismArgs(sessionConfig{}), " ")
	wmbus := strings.Join(wmbusArgs(sessionConfig{}), " ")

	if !strings.Contains(ism, "-r cf32:- -s 250000") || !strings.Contains(ism, "-M level -M stats") || strings.Contains(ism, "-R") {
		t.Errorf("ism args %s", ism)
	}

	if !strings.Contains(wmbus, "-r cf32:- -s 1200000") || !strings.Contains(wmbus, "-R 104") || !strings.Contains(wmbus, "-M level -M stats") {
		t.Errorf("wmbus args %s", wmbus)
	}
}
