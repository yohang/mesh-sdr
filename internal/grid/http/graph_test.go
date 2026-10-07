package http

import (
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
)

func TestLoadView(t *testing.T) {
	if v := newLoadView(nil); !v.Empty {
		t.Fatal("no samples: not empty")
	}

	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	var samples []app.LoadSample

	for i := range 40 {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		if i >= 20 {
			at = at.Add(5 * time.Minute) // a gap of missed heartbeats
		}

		samples = append(samples, app.LoadSample{At: at, CPU: float64(i) / 100, Load1: 0.5, MemTotalBytes: 1000, MemAvailableBytes: 750})
	}

	v := newLoadView(samples)
	if v.Empty || len(v.Charts) != 1 {
		t.Fatalf("charts = %d (no temperature: one chart)", len(v.Charts))
	}

	c := v.Charts[0]
	if len(c.Series) != 2 || len(c.Series[0].Lines) != 2 {
		t.Fatalf("series = %+v, want CPU and memory, CPU broken by the gap", c.Series)
	}

	if c.Series[0].Last != "39 %" || c.Series[0].Min != "0 %" || c.Series[1].Last != "25 %" {
		t.Errorf("CPU %+v, memory %+v", c.Series[0], c.Series[1])
	}

	if !strings.HasPrefix(c.Summary, "CPU and memory. CPU: now 39 %, lowest 0 %, highest 39 %") {
		t.Errorf("summary = %q", c.Summary)
	}

	// Points stay inside the plot area.
	for _, line := range c.Series[0].Lines {
		for _, p := range strings.Fields(line) {
			x, y, _ := strings.Cut(p, ",")
			if x < "0" || y < "0" {
				t.Errorf("point %s out of the chart", p)
			}
		}
	}

	// The table keeps one sample per 5 minutes, newest first.
	if len(v.Rows) < 2 || v.Rows[0].At != "12:11 UTC" || v.Rows[0].CPU != "39 %" || v.Rows[0].Mem != "25 %" {
		t.Errorf("rows = %+v", v.Rows)
	}

	temp := 41.5
	samples[len(samples)-1].TempC = &temp

	if v := newLoadView(samples); len(v.Charts) != 2 || v.Charts[1].Series[0].Last != "41.5 °C" {
		t.Errorf("temperature chart = %+v", v.Charts)
	}
}
