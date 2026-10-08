package decoder

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

func TestMain(m *testing.M) {
	process.MaybeRunExecHelper()
	os.Exit(m.Run())
}

func TestParseSelCall(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	for _, tc := range []struct {
		line, text, decoder, digits string
	}{
		{"ZVEI1: 12345", "[ZVEI1] 12345", "ZVEI1", "12345"},
		{"DTMF: 1", "[DTMF] 1", "DTMF", "1"},
		{"CCIR: 1E0F", "[CCIR] 1E0F", "CCIR", "1E0F"},
		{"PZVEI: 9 ", "[PZVEI] 9", "PZVEI", "9"},
		{"multimon-ng 1.3.1", "", "", ""},
		{"POCSAG1200: Address: 1", "", "", ""},
		{"ZVEI1: <script>", "", "", ""},
	} {
		rec, ok := parseSelCall(tc.line, at)
		if ok != (tc.text != "") {
			t.Errorf("%q: ok = %v", tc.line, ok)

			continue
		}

		if !ok {
			continue
		}

		var p SelCallRecord
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			t.Fatal(err)
		}

		if rec.Text != tc.text || p.Decoder != tc.decoder || p.Digits != tc.digits || rec.Schema != SelCallSchema || !rec.Time.Equal(at) {
			t.Errorf("%q: record %+v %+v", tc.line, rec, p)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.7.0", "2.4", 1}, {"2.3", "2.3.0", 0}, {"2.2.2", "2.3", -1}, {"10.0", "9.9", 1}, {"v2.4", "2.4", 0},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// supervisor returns a supervisor on a private runtime dir.
func supervisor(t *testing.T) *process.Supervisor {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	s, err := process.New(process.Options{RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

var dirs = []string{"/usr/local/bin", "/usr/bin"}

// The probe of a missing tool says which one; cap:native-dsp needs none.
func TestToolboxMissingTool(t *testing.T) {
	tb := NewToolbox(ToolboxOptions{Supervisor: supervisor(t), Tools: process.Tools{Dirs: []string{t.TempDir()}}})

	if ok, reason := tb.Available(domain.CapMultimonNG); ok || reason != "not probed yet" {
		t.Errorf("before the probe: %v %q", ok, reason)
	}

	caps := tb.Probe(context.Background())

	var mm app.DecoderCapability

	for _, c := range caps {
		if c.Cap == domain.CapMultimonNG {
			mm = c
		}
	}

	if len(mm.Tools) != 1 || mm.Tools[0].OK || mm.Tools[0].Reason != "multimon-ng not found" || strings.Join(mm.Modes, ",") != "selcall,zvei" {
		t.Errorf("cap:multimon-ng = %+v", mm)
	}

	if ok, reason := tb.Available(domain.CapMultimonNG); ok || reason != "multimon-ng not found" {
		t.Errorf("multimon-ng: %v %q", ok, reason)
	}

	if ok, _ := tb.Available(domain.CapNativeDSP); !ok {
		t.Error("cap:native-dsp unavailable")
	}
}

// The pinned tools of the images (DEC-001): probed in the dev image.
func TestToolboxProbesInstalledTools(t *testing.T) {
	if _, err := process.ResolveTool("multimon-ng", dirs); err != nil {
		t.Skip("decoder tools not installed")
	}

	tb := NewToolbox(ToolboxOptions{Supervisor: supervisor(t), Tools: process.Tools{Dirs: dirs}})
	versions := map[string]string{}

	for _, c := range tb.Probe(context.Background()) {
		ok, reason := tb.Available(c.Cap)
		if !ok {
			t.Errorf("%s unavailable: %s", c.Cap, reason)
		}

		for _, tl := range c.Tools {
			versions[tl.Name] = tl.Version
		}
	}

	for name, want := range map[string]string{"jt9": "2.7.0", "wsprd": "2.7.0", "direwolf": "1.7", "multimon-ng": "1.3.1", "rtl_433": "25.02"} {
		if versions[name] != want {
			t.Errorf("%s version = %q, want %q", name, versions[name], want)
		}
	}
}

// readWAV reads a mono 16-bit WAV fixture as float samples.
func readWAV(t *testing.T, name string) ([]float32, int) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("%s: not a WAV file", name)
	}

	rate := int(binary.LittleEndian.Uint32(b[24:28]))
	data := b[44:]
	out := make([]float32, len(data)/2)

	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(data[2*i:]))) / 32768
	}

	return out, rate
}

type events struct {
	mu       sync.Mutex
	decodes  []app.DecodeRecord
	statuses []app.DecoderStatus
}

func (e *events) sink() app.DecoderEvents {
	return app.DecoderEvents{
		Decode: func(r app.DecodeRecord) {
			e.mu.Lock()
			e.decodes = append(e.decodes, r)
			e.mu.Unlock()
		},
		Status: func(s app.DecoderStatus) {
			e.mu.Lock()
			e.statuses = append(e.statuses, s)
			e.mu.Unlock()
		},
	}
}

func (e *events) texts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []string
	for _, d := range e.decodes {
		out = append(out, d.Text)
	}

	return out
}

// A ZVEI sequence recorded at the 12 kHz listener rate decodes through
// multimon-ng (DEC-035): the session resamples it to 22 050 Hz, runs the
// tool in its workdir and reports running.
func TestZVEIDecodesFixture(t *testing.T) {
	if _, err := process.ResolveTool("multimon-ng", dirs); err != nil {
		t.Skip("multimon-ng not installed")
	}

	samples, rate := readWAV(t, "zvei1-12345.wav")
	sup := supervisor(t)
	r := NewRunner(Options{Supervisor: sup, Tools: process.Tools{Dirs: dirs}})

	mode, err := domain.DigitalModeOf("zvei")
	if err != nil {
		t.Fatal(err)
	}

	var ev events

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("0190c8a4-0000-7000-8000-000000000001"), Mode: mode}, ev.sink())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	// 20 ms blocks, like the demodulator.
	block := rate / 50
	for i := 0; i < len(samples); i += block {
		run.Audio(app.AudioBlock{Samples: samples[i:min(i+block, len(samples))], Rate: rate, Time: time.Now()})
	}

	// Trailing silence flushes the decoder.
	run.Audio(app.AudioBlock{Samples: make([]float32, rate), Rate: rate})

	// One multimon-ng per ZVEI variant: each reports what it hears.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !slices.Contains(ev.texts(), "[ZVEI1] 12345") {
		time.Sleep(20 * time.Millisecond)
	}

	if got := ev.texts(); !slices.Contains(got, "[ZVEI1] 12345") || !slices.Contains(got, "[ZVEI2] 12345") {
		t.Errorf("decodes = %q", got)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.statuses) == 0 || ev.statuses[0].State != app.DecoderRunning {
		t.Errorf("statuses = %+v", ev.statuses)
	}
}

// A decoder whose tool is gone reports unavailable and asks for a probe.
func TestMissingToolUnavailable(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "multimon-ng")

	reprobed := make(chan struct{}, 1)
	r := NewRunner(Options{
		Supervisor: supervisor(t), Tools: process.Tools{Paths: map[string]string{"multimon-ng": tool}, Dirs: []string{dir}},
		Reprobe: func() {
			select {
			case reprobed <- struct{}{}:
			default:
			}
		},
	})

	mode, _ := domain.DigitalModeOf("selcall")

	var ev events

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("0190c8a4-0000-7000-8000-000000000002"), Mode: mode}, ev.sink())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	select {
	case <-reprobed:
	case <-time.After(5 * time.Second):
		t.Fatal("no re-probe")
	}

	// Every process of the session reports; the session says it once.
	time.Sleep(100 * time.Millisecond)

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.statuses) != 1 || ev.statuses[0] != (app.DecoderStatus{State: app.DecoderUnavailable, Reason: "tool_missing"}) {
		t.Errorf("statuses = %+v", ev.statuses)
	}
}
