package devlog_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/radio/infra/devlog"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

var epoch = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func now() time.Time { return epoch }

func line(text string) process.Line {
	return process.Line{Time: epoch, Class: process.ClassInfo, Text: text}
}

func TestRingBound(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		lines int
		want  int
	}{
		{name: "default size, under", lines: 10, want: 10},
		{name: "default size, at", lines: devlog.DefaultSize, want: devlog.DefaultSize},
		{name: "default size, over", lines: devlog.DefaultSize + 57, want: devlog.DefaultSize},
		{name: "small ring", size: 3, lines: 5, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := devlog.New([]string{"hf"}, tt.size, now)

			for i := range tt.lines {
				l.Connector("hf", line("line "+strconv.Itoa(i)))
			}

			got := l.Records("hf")
			if len(got) != tt.want {
				t.Fatalf("records = %d, want %d", len(got), tt.want)
			}

			// The newest records are kept, oldest first.
			if last := got[len(got)-1].Text; last != "line "+strconv.Itoa(tt.lines-1) {
				t.Errorf("last = %q", last)
			}

			if first := got[0].Text; first != "line "+strconv.Itoa(tt.lines-tt.want) {
				t.Errorf("first = %q", first)
			}
		})
	}
}

func TestRecords(t *testing.T) {
	l := devlog.New([]string{"hf", "vhf"}, 0, now)

	l.State("hf", "starting", "")
	l.State("hf", "starting", "") // unchanged: not recorded
	l.Connector("hf", process.Line{Time: epoch, Class: process.ClassDeviceLost, Text: "usb_claim_interface error -6", Truncated: true})
	l.State("hf", "retry_wait", "sample_stall")
	l.Connector("nope", line("unknown device"))
	l.Connector("vhf", line(strings.Repeat("é", ctl.MaxLogText)))

	want := []ctl.LogRecord{
		{Time: epoch.UnixMilli(), Source: ctl.LogSourceDevice, Class: "starting", Text: "state starting"},
		{Time: epoch.UnixMilli(), Source: ctl.LogSourceConnector, Class: "device_lost", Text: "usb_claim_interface error -6 …"},
		{Time: epoch.UnixMilli(), Source: ctl.LogSourceDevice, Class: "retry_wait", Text: "state retry_wait (sample_stall)"},
	}

	got := l.Records("hf")
	if len(got) != len(want) {
		t.Fatalf("records = %+v", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if l.Records("nope") != nil {
		t.Error("records of an unknown device")
	}

	if v := l.Records("vhf"); len(v) != 1 || len(v[0].Text) > ctl.MaxLogText || !strings.HasPrefix(v[0].Text, "é") {
		t.Errorf("capped text = %d bytes", len(v[0].Text))
	}
}

func TestBacklogAndSince(t *testing.T) {
	l := devlog.New([]string{"vhf", "hf"}, 0, now)

	changed := l.Changed()
	l.Connector("hf", line("one"))

	select {
	case <-changed:
	default:
		t.Fatal("Changed not closed by a record")
	}

	backlog, cursor := l.Backlog()
	if len(backlog) != 2 || backlog[0].DeviceID != "hf" || !backlog[0].Reset || len(backlog[0].Records) != 1 ||
		backlog[1].DeviceID != "vhf" || !backlog[1].Reset || len(backlog[1].Records) != 0 {
		t.Fatalf("backlog = %+v", backlog)
	}

	if msgs, next := l.Since(cursor); msgs != nil || next != cursor {
		t.Errorf("since, nothing new = %+v %d", msgs, next)
	}

	l.Connector("vhf", line("two"))
	l.Connector("hf", line("three"))

	msgs, next := l.Since(cursor)
	if len(msgs) != 2 || msgs[0].Reset || msgs[0].DeviceID != "hf" || msgs[0].Records[0].Text != "three" ||
		msgs[1].DeviceID != "vhf" || msgs[1].Records[0].Text != "two" || next != cursor+2 {
		t.Errorf("since = %+v %d", msgs, next)
	}
}

func TestBacklogChunks(t *testing.T) {
	l := devlog.New([]string{"hf"}, 0, now)

	// Escaped by JSON (<): the largest records.
	for range devlog.DefaultSize {
		l.Connector("hf", line(strings.Repeat("<", ctl.MaxLogText)))
	}

	backlog, _ := l.Backlog()
	if len(backlog) < 2 {
		t.Fatalf("backlog in %d message(s)", len(backlog))
	}

	total := 0

	for i, m := range backlog {
		if m.Reset != (i == 0) {
			t.Errorf("message %d reset = %v", i, m.Reset)
		}

		raw, _ := json.Marshal(m)
		if len(raw) > 60<<10 {
			t.Errorf("message %d = %d bytes", i, len(raw))
		}

		total += len(m.Records)
	}

	if total != devlog.DefaultSize {
		t.Errorf("records = %d", total)
	}
}
