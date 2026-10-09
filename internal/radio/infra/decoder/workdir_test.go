package decoder

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// A workdir that cannot be created is reported as an error (DEC-048).
func TestWorkdirFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	sup, err := process.New(process.Options{RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	id := "0190c8a4-0000-7000-8000-000000000003"

	// The session's workdir already exists: its exclusive creation fails.
	if err := os.Mkdir(filepath.Join(dir, "sessions", "dec-"+id), 0o700); err != nil {
		t.Fatal(err)
	}

	tool := filepath.Join(t.TempDir(), "multimon-ng")
	r := NewRunner(Options{Supervisor: sup, Tools: process.Tools{Paths: map[string]string{"multimon-ng": tool}}})
	mode, _ := domain.DigitalModeOf("selcall")

	var ev events

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID(id), Mode: mode, Variant: "DTMF"}, ev.sink())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		ev.mu.Lock()
		n := len(ev.statuses)
		ev.mu.Unlock()

		if n > 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.statuses) != 1 || ev.statuses[0] != (app.DecoderStatus{State: app.DecoderError, Reason: "workdir"}) {
		t.Errorf("statuses = %+v", ev.statuses)
	}

	// A closed session reports nothing more.
	s := run.(*toolSession)
	run.Close()
	s.decode(app.DecodeRecord{Text: "late"})
	s.status(app.DecoderStatus{State: app.DecoderRunning})

	if len(ev.decodes) != 0 || len(ev.statuses) != 1 {
		t.Errorf("after close: %+v %+v", ev.decodes, ev.statuses)
	}
}
