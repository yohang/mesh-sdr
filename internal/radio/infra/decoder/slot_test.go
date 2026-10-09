package decoder

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Real output lines of the pinned tools (wsjtx 2.7.0, fixtures made with
// ft8sim, fst4sim, q65sim and jt65sim).
func TestParseJT9(t *testing.T) {
	ft8 := profiles("ft8", Settings{})[0]
	fst4w := profile{mode: "fst4w", period: 2 * time.Minute}
	q65 := profile{mode: "q65", period: 30 * time.Second, submode: "A"}

	for _, tc := range []struct {
		name string
		p    profile
		line string
		want *WSJTRecord
	}{
		{"ft8 cq", ft8, "120015 -12  0.0 1200 ~  CQ DL1ABC JO62                          ",
			&WSJTRecord{Mode: "FT8", Period: 15, DB: -12, DF: 1200, Msg: "CQ DL1ABC JO62", Callsign: "DL1ABC", Locator: "JO62"}},
		{"ft8 rr73", ft8, "120030 -12  0.0  800 ~  K1ABC W9XYZ RR73                        ",
			&WSJTRecord{Mode: "FT8", Period: 15, DB: -12, DF: 800, Msg: "K1ABC W9XYZ RR73", Callsign: "W9XYZ", Callee: "K1ABC"}},
		{"fst4", profile{mode: "fst4", period: time.Minute}, "1201 -10  0.0 1500 `  CQ K1ABC FN42                                   ",
			&WSJTRecord{Mode: "FST4", Period: 60, DB: -10, DF: 1500, Msg: "CQ K1ABC FN42", Callsign: "K1ABC", Locator: "FN42"}},
		{"fst4w beacon", fst4w, "1202 -10  0.0 1500 `  K1ABC FN42 33                                   ",
			&WSJTRecord{Mode: "FST4W", Period: 120, DB: -10, DF: 1500, Msg: "K1ABC FN42 33", Callsign: "K1ABC", Locator: "FN42", DBm: ptr(33)}},
		{"q65 annotation", q65, "120030 -11  0.0 1500 :  K1ABC W9XYZ EN37                      q3 ",
			&WSJTRecord{Mode: "Q65", Period: 30, Submode: "A", DB: -11, DF: 1500, Msg: "K1ABC W9XYZ EN37", Callsign: "W9XYZ", Locator: "EN37"}},
		{"jt65", profile{mode: "jt65", period: time.Minute}, "1203 -20 -0.0  600 #  K1ABC W9XYZ EN37          ",
			&WSJTRecord{Mode: "JT65", Period: 60, DB: -20, DF: 600, Msg: "K1ABC W9XYZ EN37", Callsign: "W9XYZ", Locator: "EN37"}},
		{"q65 empty", q65, "120030 -11  0.0 1500 :                                        q0 ", nil},
		{"finished", ft8, "<DecodeFinished>   0   1        0", nil},
		{"eof", ft8, " EOF on input file", nil},
		{"garbage", ft8, "hello world", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := parseJT9(tc.p, tc.line, false)
			if ok != (tc.want != nil) {
				t.Fatalf("ok = %v", ok)
			}

			if !ok {
				return
			}

			var got WSJTRecord
			if err := json.Unmarshal(rec.Payload, &got); err != nil {
				t.Fatal(err)
			}

			if !sameJSON(t, got, *tc.want) || rec.Text != tc.want.Msg || rec.Schema != WSJTSchema || rec.AudioHz != tc.want.DF {
				t.Errorf("record %+v %+v", rec, got)
			}
		})
	}
}

func TestParseWSPR(t *testing.T) {
	p := profiles("wspr", Settings{})[0]

	for _, tc := range []struct {
		line string
		df   int64
	}{
		{"1200 -20 -0.0   0.001500  0  K1ABC FN42 33 ", 1500},
		{"0052 -29  2.6   0.001486 -1  G0ABC IO92 23", 1486},
	} {
		rec, ok := parseWSPR(p, tc.line, true)
		if !ok {
			t.Fatalf("%q not parsed", tc.line)
		}

		var got WSJTRecord
		_ = json.Unmarshal(rec.Payload, &got)

		if got.DF != tc.df || rec.AudioHz != tc.df || got.Drift == nil || got.DBm == nil || got.Locator == "" || !got.PartialSlot || got.Mode != "WSPR" {
			t.Errorf("%q: %+v", tc.line, got)
		}
	}

	if _, ok := parseWSPR(p, "<DecodeFinished>", false); ok {
		t.Error("DecodeFinished parsed")
	}
}

// js8py's test vectors (real js8 lines).
func TestParseJS8(t *testing.T) {
	for _, tc := range []struct {
		line, text, frame string
	}{
		{"183500 -22  0.5 1519 A  iXvZfxW3Sju+         2", "KALHSPERA VNE", JS8Data},
		{"201115 -16  0.6 1075 A  YNlWl7V+Uw-j         0", "F DIGI TOO DM7", JS8Data},
		{"140000 -11  0.4 1050 A  qBdgE+EP++++         2", "IN ITALY TODAY", JS8DataCompressed},
		{"151615 -11  0.3 2457 A  ViZoThL+C+aL         3", "G0CQZ: DF4MJ SNR -10 ", JS8Directed},
		{"161630 -19  0.3 1203 A  QrqjshWc6lq0         3", "DG3EK: DM5CQ ACK ", JS8Directed},
		{"165430 -14  2.5 1697 A  Tuj1fVGGPoy0         1", "RV4CQ: @APRSIS GRID ", JS8Directed},
		{"201915 -13  0.3  794 A  R43a8hMPfZqV         3", "EI2GYB: DF7FR ACK +00 ", JS8Directed},
		{"183445 -24  0.3 1520 A  VkrOrOOSTgK0         1", "M0SUY: SV1GGY> ", JS8Directed},
		{"172300 -12  0.4 1044 A  2jNlWSPIPQ-W         3", "LZ1CWK: CQ CQ CQ KN32", JS8Heartbeat},
		{"192415 -15  0.4  953 A  1-ckNKyPOVwh         3", "G0CQZ: HB AUTO RELAY SPOT IO91", JS8Heartbeat},
		{"200415 -13  0.5 1023 A  1mqQJL8Bv++u         3", "EA3ENR: CQ CQ CQ ", JS8Heartbeat},
		{"063545 -10 -0.2 1164 A  BYh0otuHOS3G         1", "SP5GSM:", JS8Compound},
		{"211315 -15  0.6 1250 A  B8giaUqYuUtG         1", "PE75OUW:", JS8Compound},
		{"200345 -12  0.8 1001 A  H-jnbQvYe+ke         2", "G0WZM/A ACK -04", JS8CompoundDirected},
	} {
		rec, ok := parseJS8(profile{}, tc.line, false)
		if !ok {
			t.Errorf("%q not parsed", tc.line)

			continue
		}

		var got JS8Record
		_ = json.Unmarshal(rec.Payload, &got)

		if rec.Text != tc.text || got.Frame != tc.frame || rec.Schema != JS8Schema || got.Submode != "A" || rec.AudioHz != got.DF || got.DF == 0 {
			t.Errorf("%q: %q %+v", tc.line, rec.Text, got)
		}
	}

	rec, _ := parseJS8(profile{}, "172300 -12  0.4 1044 A  2jNlWSPIPQ-W         3", false)

	var hb JS8Record
	_ = json.Unmarshal(rec.Payload, &hb)

	if hb.Callsign != "LZ1CWK" || hb.Locator != "KN32" || hb.ThreadType != 3 || hb.DB != -12 || hb.DT != 0.4 {
		t.Errorf("heartbeat %+v", hb)
	}

	for _, line := range []string{"<DecodeStarted>", " <DecodeDebug> x", "183500 -22  0.5 1519 A  iXvZfx         2", "183500 -22  0.5 1519 A  iXvZfx{W3Sju+         2"} {
		if _, ok := parseJS8(profile{}, line, false); ok {
			t.Errorf("%q parsed", line)
		}
	}
}

func TestProfiles(t *testing.T) {
	args := func(mode string, s Settings) []string {
		var out []string
		for _, p := range profiles(mode, s) {
			out = append(out, p.tool+" "+strings.Join(p.args("f.wav", "J"), " ")+" @"+p.period.String()+"/"+p.job.String())
		}

		return out
	}

	for _, tc := range []struct {
		mode string
		s    Settings
		want []string
	}{
		{"ft8", Settings{}, []string{"jt9 --ft8 -a J -t J -d 3 f.wav @15s/13s"}},
		{"ft4", Settings{WSJTDepth: 2}, []string{"jt9 --ft4 -a J -t J -d 2 f.wav @7.5s/6s"}},
		{"jt65", Settings{}, []string{"jt9 --jt65 -a J -t J -d 3 f.wav @1m0s/50s"}},
		{"jt65", Settings{WSJTDepth: 3, WSJTDepths: map[string]int{"jt65": 1}}, []string{"jt9 --jt65 -a J -t J -d 1 f.wav @1m0s/50s"}},
		{"jt65", Settings{WSJTDepth: 2, WSJTDepths: map[string]int{"ft8": 1}}, []string{"jt9 --jt65 -a J -t J -d 2 f.wav @1m0s/50s"}},
		{"jt9", Settings{WSJTDepths: map[string]int{"jt9": 1}}, []string{"jt9 --jt9 -a J -t J -d 1 f.wav @1m0s/50s"}},
		{"wspr", Settings{}, []string{"wsprd -d f.wav @2m0s/1m40s"}},
		{"wspr", Settings{WSJTDepth: 1}, []string{"wsprd f.wav @2m0s/1m40s"}},
		{"fst4", Settings{FST4Intervals: []int{15, 30}}, []string{"jt9 --fst4 -p 15 -a J -t J -d 3 f.wav @15s/12s", "jt9 --fst4 -p 30 -a J -t J -d 3 f.wav @30s/24s"}},
		{"fst4", Settings{FST4Intervals: []int{1800, 7, 60}}, []string{"jt9 --fst4 -p 60 -a J -t J -d 3 f.wav @1m0s/48s", "jt9 --fst4 -p 1800 -a J -t J -d 3 f.wav @30m0s/24m0s"}},
		{"fst4w", Settings{FST4WIntervals: []int{120, 300}}, []string{"jt9 --fst4w -p 120 -a J -t J -d 3 f.wav @2m0s/1m36s", "jt9 --fst4w -p 300 -a J -t J -d 3 f.wav @5m0s/4m0s"}},
		{"q65", Settings{Q65Combinations: []string{"A30", "E15", "C60"}}, []string{"jt9 --q65 -p 30 -b A -a J -t J -d 3 f.wav @30s/24s", "jt9 --q65 -p 60 -b C -a J -t J -d 3 f.wav @1m0s/48s"}},
		{"js8", Settings{JS8Profiles: []string{"normal", "slow"}}, []string{"js8 --js8 -b A -a J -t J -d 3 f.wav @15s/12s", "js8 --js8 -b E -a J -t J -d 3 f.wav @30s/24s"}},
		{"js8", Settings{JS8Profiles: []string{"turbo", "fast"}, JS8Depth: 1}, []string{"js8 --js8 -b B -a J -t J -d 1 f.wav @10s/8s", "js8 --js8 -b C -a J -t J -d 1 f.wav @6s/4.8s"}},
		{"selcall", Settings{}, nil},
	} {
		if got := args(tc.mode, tc.s); !slices.Equal(got, tc.want) {
			t.Errorf("%s %+v:\n got %q\nwant %q", tc.mode, tc.s, got, tc.want)
		}
	}
}

func TestQ65Combinations(t *testing.T) {
	want := "A15 B15 C15 A30 B30 C30 D30 A60 B60 C60 D60 E60 A120 B120 C120 D120 E120 A300 B300 C300 D300 E300"
	if got := strings.Join(q65Combinations(), " "); got != want {
		t.Errorf("got %s", got)
	}
}

// Every mode of the catalogue has a decoder: the image and text decoders
// of the node, slot profiles or a tool reading its input.
func TestCatalogueDecoders(t *testing.T) {
	all := Settings{
		FST4Intervals: fst4Periods, FST4WIntervals: fst4wPeriods, Q65Combinations: q65Combinations(), JS8Profiles: js8SpeedOrder,
	}

	for _, m := range domain.DigitalModes() {
		var ok bool

		switch m.Family {
		case domain.FamilyImage:
			ok = m.Name == app.FileSSTV || m.Name == app.FileFAX
		case domain.FamilyTextModes, domain.FamilyDSC:
			_, ok = textModes[m.Name]
		case domain.FamilyWSJT, domain.FamilyJS8:
			ok = m.Slot > 0 && m.Input == domain.InputAudio && len(profiles(m.Name, all)) > 0
		default:
			ts, found := toolSpecs[m.Name]
			ok = found && (ts.iq == iqNone) == (m.Input == domain.InputAudio)
		}

		if !ok {
			t.Errorf("%s (%s) has no decoder", m.Name, m.Family)
		}
	}

	// The text decoders and their secondary FFT run at one rate.
	if domain.TextRate != dsp.TextRate {
		t.Errorf("domain.TextRate %d, dsp.TextRate %d", domain.TextRate, dsp.TextRate)
	}
}

func TestQueue(t *testing.T) {
	now := time.Now()
	q := NewQueue(1, 2, func() time.Time { return now }, nil)

	var (
		mu      sync.Mutex
		events  []string
		started = make(chan struct{})
		release = make(chan struct{})
	)

	record := func(s string) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	}

	job := func(name string, deadline time.Time, block bool) Job {
		return Job{
			Deadline: deadline,
			Run: func(context.Context, time.Duration) {
				record("run " + name)

				if block {
					close(started)
					<-release
				}
			},
			Drop: func(reason string) { record("drop " + name + " " + reason) },
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { q.Run(ctx); close(done) }()

	q.Put(job("a", now.Add(time.Minute), true))
	<-started

	q.Put(job("b", now.Add(time.Minute), false))
	q.Put(job("c", now.Add(-time.Second), false))
	q.Put(job("d", now.Add(time.Minute), false)) // the queue holds 2: b is dropped

	if q.Depth() != 2 {
		t.Errorf("depth %d", q.Depth())
	}

	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for q.Depth() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()

	want := []string{"run a", "drop b queue_overflow", "drop c job_timeout", "run d"}
	if !slices.Equal(events, want) {
		t.Errorf("events %q, want %q", events, want)
	}
}

// A closed session's waiting jobs are dropped; the jobs left when the node
// stops are dropped too (their slot files go).
func TestQueuePurgeAndStop(t *testing.T) {
	q := NewQueue(1, 8, nil, nil)

	var (
		mu      sync.Mutex
		dropped []string
	)

	job := func(owner any, name string) Job {
		return Job{
			Owner: owner, Deadline: time.Now().Add(time.Minute),
			Run: func(context.Context, time.Duration) { t.Errorf("%s ran", name) },
			Drop: func(reason string) {
				mu.Lock()
				dropped = append(dropped, name+" "+reason)
				mu.Unlock()
			},
		}
	}

	a, b := new(int), new(int)
	q.Put(job(a, "a1"))
	q.Put(job(b, "b1"))
	q.Put(job(a, "a2"))
	q.Purge(a)

	if q.Depth() != 1 {
		t.Errorf("depth %d", q.Depth())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q.Run(ctx)

	mu.Lock()
	defer mu.Unlock()

	if want := []string{"a1 stopped", "a2 stopped", "b1 stopped"}; !slices.Equal(dropped, want) || q.Depth() != 0 {
		t.Errorf("dropped %q", dropped)
	}
}

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(dir, func() int64 { return 14_074_000 }, discard())
	start := time.Date(2026, 10, 8, 12, 0, 30, 0, time.UTC)
	periods := []time.Duration{15 * time.Second, 30 * time.Second}

	block := make([]byte, 2*domain.SlotRate/10) // 100 ms
	for i := range len(block) / 2 {
		binary.LittleEndian.PutUint16(block[2*i:], 1000)
	}

	var done []*slotFile

	// 12:00:31 to 12:00:46, with a 3 s hole from 12:00:35.
	for t0 := start.Add(time.Second); t0.Before(start.Add(16 * time.Second)); t0 = t0.Add(100 * time.Millisecond) {
		if t0.Sub(start) >= 5*time.Second && t0.Sub(start) < 8*time.Second {
			continue
		}

		done = append(done, r.write(t0, block, periods)...)
	}

	if len(done) != 1 || done[0].period != 15*time.Second || !done[0].start.Equal(start) {
		t.Fatalf("done %+v", done)
	}

	f := done[0]
	if filepath.Base(f.path) != "p15000_261008_120030.wav" || f.real != 11*domain.SlotRate || !f.partial() || f.dial != 14_074_000 {
		t.Errorf("slot %s real %d partial %v", filepath.Base(f.path), f.real, f.partial())
	}

	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}

	data := binary.LittleEndian.Uint32(b[40:44])
	if string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint32(b[24:28]) != domain.SlotRate || int(data) != len(b)-wavHeader ||
		data != 2*15*domain.SlotRate {
		t.Errorf("header %x, %d bytes", b[:44], len(b))
	}

	// The first second and the hole read as silence, the rest as audio.
	at := func(sec float64) uint16 { return binary.LittleEndian.Uint16(b[wavHeader+2*int(sec*domain.SlotRate):]) }
	if at(0.5) != 0 || at(1.5) != 1000 || at(6) != 0 || at(9) != 1000 {
		t.Errorf("samples %d %d %d %d", at(0.5), at(1.5), at(6), at(9))
	}

	// The 30 s slot is still open; dropping its period discards it.
	if len(r.open) != 2 {
		t.Errorf("open %d", len(r.open))
	}

	r.write(start.Add(16*time.Second), block, nil)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || len(r.open) != 0 {
		t.Errorf("files %v open %d", entries, len(r.open))
	}

	f.refs.Store(2)
	f.release(discard())

	if _, err := os.Stat(f.path); err != nil {
		t.Error("removed before its last job")
	}

	f.release(discard())

	if _, err := os.Stat(f.path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("not removed after its last job")
	}
}

// Timestamps going back into a finished slot: its file still exists (its
// jobs wait), so that slot is skipped, once, until the next one; audio
// written twice counts once.
func TestRecorderBackwards(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(dir, nil, discard())
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	periods := []time.Duration{15 * time.Second}
	block := make([]byte, 2*domain.SlotRate) // 1 s

	r.write(start, block, periods)
	r.write(start, block, periods) // the same second again

	if f := r.open[15*time.Second]; f == nil || f.real != domain.SlotRate {
		t.Fatalf("open %+v", r.open)
	}

	done := r.write(start.Add(15*time.Second), block, periods)
	if len(done) != 1 {
		t.Fatalf("done %d", len(done))
	}

	// A recorder meeting the file of a slot that still exists skips that
	// slot, then the next slot goes on.
	r2 := newRecorder(dir, nil, discard())
	for range 3 {
		if got := r2.write(start.Add(2*time.Second), block, periods); len(got) != 0 || r2.open[15*time.Second] != nil {
			t.Fatalf("done %d open %+v", len(got), r2.open)
		}
	}

	if !r2.failed[15*time.Second].Equal(start) {
		t.Errorf("failed %v", r2.failed)
	}

	r2.write(start.Add(46*time.Second), block, periods)

	if f := r2.open[15*time.Second]; f == nil || !f.start.Equal(start.Add(45*time.Second)) {
		t.Errorf("next slot not open: %+v", r2.open)
	}
}

func TestSlotFileName(t *testing.T) {
	at := time.Date(2026, 10, 8, 23, 59, 30, 0, time.UTC)
	if got := slotFileName(at, 7500*time.Millisecond); got != "p7500_261008_235930.wav" {
		t.Error(got)
	}

	if got := slotFileName(at, 2*time.Minute); got != "p120000_261008_2359.wav" {
		t.Error(got)
	}
}

// TestFT8SlotEndToEnd: a slot of FT8 audio (made by ft8sim) fed as
// timestamped audio is cut at the UTC slot boundary, decoded by jt9 through
// the queue, reported with the slot's date, and the workdir is removed
// when the session closes (DEC-016, DEC-025, DEC-026, DEC-027).
func TestFT8SlotEndToEnd(t *testing.T) {
	sim, err := process.ResolveTool("ft8sim", dirs)
	if err != nil {
		t.Skip("ft8sim not installed")
	}

	if _, err := process.ResolveTool("jt9", dirs); err != nil {
		t.Skip("jt9 not installed")
	}

	simDir := t.TempDir()
	cmd := exec.Command(sim, "CQ K1ABC FN42", "1500", "0.0", "0.0", "0.0", "1", "-10")
	cmd.Dir = simDir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ft8sim: %v %s", err, out)
	}

	wav, err := os.ReadFile(filepath.Join(simDir, "000000_000001.wav"))
	if err != nil {
		t.Fatal(err)
	}

	pcm := wav[wavHeader:]
	audio := make([]float32, len(pcm)/2)

	for i := range audio {
		audio[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}

	sup := supervisor(t)
	queue := NewQueue(1, 4, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	go queue.Run(ctx)

	r := NewRunner(Options{Supervisor: sup, Tools: process.Tools{Dirs: dirs}, Queue: queue, ClockSynced: func() bool { return false }})

	mode, err := domain.DigitalModeOf("ft8")
	if err != nil {
		t.Fatal(err)
	}

	decodes := make(chan app.DecodeRecord, 4)
	statuses := make(chan app.DecoderStatus, 16)
	id := shared.MustParseUUID("01890000-0000-7000-8000-000000000001")

	run, err := r.Start(app.DecoderSpec{Session: id, Mode: mode, Dial: func() int64 { return 14_074_000 }}, app.DecoderEvents{
		Decode: func(rec app.DecodeRecord) { decodes <- rec },
		Status: func(st app.DecoderStatus) { statuses <- st },
	})
	if err != nil {
		t.Fatal(err)
	}

	defer run.Close()

	// The current slot: its deadline (end + 13 s) is ahead.
	start := slotStart(time.Now(), 15*time.Second)

	const step = domain.SlotRate / 50
	for i := 0; i < len(audio); i += step {
		run.Audio(app.AudioBlock{Samples: audio[i:min(i+step, len(audio))], Rate: domain.SlotRate, Time: start.Add(durationOf(i))})
		time.Sleep(200 * time.Microsecond)
	}

	run.Audio(app.AudioBlock{Samples: make([]float32, step), Rate: domain.SlotRate, Time: start.Add(15 * time.Second)})

	// Running from the first audio, with the clock warning.
	if st := <-statuses; st.State != app.DecoderRunning || st.Warning != app.WarningClock {
		t.Errorf("first status %+v", st)
	}

	select {
	case rec := <-decodes:
		var p WSJTRecord
		_ = json.Unmarshal(rec.Payload, &p)

		if rec.Text != "CQ K1ABC FN42" || !rec.Time.Equal(start) || rec.DialHz != 14_074_000 || p.Callsign != "K1ABC" || p.Locator != "FN42" || rec.AudioHz < 1490 || rec.AudioHz > 1510 {
			t.Errorf("decode %+v %+v", rec, p)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no decode")
	}

	wd := run.(*slotSession).dir

	run.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(wd); errors.Is(err, fs.ErrNotExist) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Error("workdir not removed")
}

func ptr[T any](v T) *T { return &v }

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()

	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)

	return string(x) == string(y)
}
