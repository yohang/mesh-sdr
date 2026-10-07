package http

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
)

// The load history graph of a node (GRID-009) is inline SVG rendered on the
// server: no chart library, no script, and the nonce-only CSP allows it
// (SVG presentation attributes are not style attributes). Colours come from
// the theme tokens through classes. A data table carries the same figures
// for screen readers and keyboard users.

// Chart geometry, in viewBox units.
const (
	chartWidth  = 600
	chartHeight = 160
	chartLeft   = 44 // room for the y labels
	chartRight  = 8
	chartTop    = 8
	chartBottom = 24 // room for the x labels
	// chartGap breaks a line where heartbeats are missing.
	chartGap = time.Minute
	// tableStep is the period of the rows of the data table.
	tableStep = 5 * time.Minute
)

// series is one line of a chart.
type series struct {
	Label string
	Class string // stroke colour and dash (theme tokens)
	// Lines are SVG polyline point lists, one per run without a gap.
	Lines []string
	Last  string
	Min   string
	Max   string
}

// tick is a labelled grid line.
type tick struct {
	Pos   string
	Label string
}

// chart is a time chart of the load history.
type chart struct {
	ID     string
	Title  string
	Unit   string
	Series []series
	YTicks []tick
	XTicks []tick
	// Summary describes the chart for people who do not see it.
	Summary string
}

// point is one sample of a series; ok is false when the sample has no
// value (no temperature sensor).
type point struct {
	at time.Time
	v  float64
	ok bool
}

// loadView is the history of a node, ready to render.
type loadView struct {
	Charts []chart
	Rows   []loadRow
	Since  string
	Empty  bool
}

// loadRow is one row of the data table.
type loadRow struct {
	At, CPU, Load, Mem, Temp string
}

func pct(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + " %" }

func num(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

func clock(t time.Time) string { return t.UTC().Format("15:04") }

func memUsed(s app.LoadSample) (float64, bool) {
	if s.MemTotalBytes == 0 || s.MemAvailableBytes > s.MemTotalBytes {
		return 0, false
	}

	return 100 * float64(s.MemTotalBytes-s.MemAvailableBytes) / float64(s.MemTotalBytes), true
}

// cpuPercent converts the CPU busy ratio of a heartbeat (0–1) to percent.
func cpuPercent(v float64) float64 { return 100 * v }

// newLoadView builds the charts and the table of samples (oldest first).
func newLoadView(samples []app.LoadSample) loadView {
	if len(samples) == 0 {
		return loadView{Empty: true}
	}

	cpu := make([]point, len(samples))
	mem := make([]point, len(samples))
	temp := make([]point, len(samples))
	hasTemp := false

	for i, s := range samples {
		cpu[i] = point{at: s.At, v: cpuPercent(s.CPU), ok: true}

		m, ok := memUsed(s)
		mem[i] = point{at: s.At, v: m, ok: ok}

		if s.TempC != nil {
			temp[i] = point{at: s.At, v: *s.TempC, ok: true}
			hasTemp = true
		}
	}

	from, to := samples[0].At, samples[len(samples)-1].At
	if !to.After(from) {
		to = from.Add(time.Minute)
	}

	v := loadView{Since: from.UTC().Format("2006-01-02 15:04 UTC"), Rows: loadRows(samples)}

	load := chart{ID: "load", Title: "CPU and memory", Unit: "%", YTicks: yTicks(0, 100, "%"), XTicks: xTicks(from, to)}
	load.Series = []series{
		line("CPU", "stroke-accent", cpu, from, to, 0, 100, pct),
		line("Memory in use", "stroke-warning [stroke-dasharray:6_4]", mem, from, to, 0, 100, pct),
	}
	load.Summary = summary(load)
	v.Charts = append(v.Charts, load)

	if hasTemp {
		lo, hi := bounds(temp)
		c := chart{ID: "temperature", Title: "Temperature", Unit: "°C", YTicks: yTicks(lo, hi, " °C"), XTicks: xTicks(from, to)}
		c.Series = []series{line("Temperature", "stroke-danger", temp, from, to, lo, hi, func(f float64) string { return num(f, 1) + " °C" })}
		c.Summary = summary(c)
		v.Charts = append(v.Charts, c)
	}

	return v
}

// bounds returns a y range around the values, on whole tens.
func bounds(ps []point) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)

	for _, p := range ps {
		if p.ok {
			lo, hi = math.Min(lo, p.v), math.Max(hi, p.v)
		}
	}

	lo, hi = math.Floor(lo/10)*10, math.Ceil(hi/10)*10
	if hi <= lo {
		hi = lo + 10
	}

	return lo, hi
}

func xOf(t, from, to time.Time) float64 {
	w := float64(chartWidth - chartLeft - chartRight)

	return chartLeft + w*float64(t.Sub(from))/float64(to.Sub(from))
}

func yOf(v, lo, hi float64) float64 {
	h := float64(chartHeight - chartTop - chartBottom)
	v = math.Max(lo, math.Min(hi, v))

	return chartTop + h*(1-(v-lo)/(hi-lo))
}

func coord(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// line builds a series: one polyline per run of samples without a gap.
func line(label, class string, ps []point, from, to time.Time, lo, hi float64, format func(float64) string) series {
	s := series{Label: label, Class: class}

	var (
		b        strings.Builder
		prev     time.Time
		min, max = math.Inf(1), math.Inf(-1)
		last     *point
	)

	flush := func() {
		if b.Len() > 0 {
			s.Lines = append(s.Lines, b.String())
			b.Reset()
		}
	}

	for i := range ps {
		p := ps[i]
		if !p.ok {
			flush()

			continue
		}

		if !prev.IsZero() && p.at.Sub(prev) > chartGap {
			flush()
		}

		if b.Len() > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(coord(xOf(p.at, from, to)) + "," + coord(yOf(p.v, lo, hi)))

		prev, last = p.at, &ps[i]
		min, max = math.Min(min, p.v), math.Max(max, p.v)
	}

	flush()

	if last != nil {
		s.Last, s.Min, s.Max = format(last.v), format(min), format(max)
	}

	return s
}

func yTicks(lo, hi float64, unit string) []tick {
	out := make([]tick, 0, 3)

	for _, v := range []float64{lo, (lo + hi) / 2, hi} {
		out = append(out, tick{Pos: coord(yOf(v, lo, hi)), Label: num(v, 0) + unit})
	}

	return out
}

func xTicks(from, to time.Time) []tick {
	return []tick{
		{Pos: coord(xOf(from, from, to)), Label: clock(from)},
		{Pos: coord(xOf(to, from, to)), Label: clock(to) + " UTC"},
	}
}

func summary(c chart) string {
	parts := make([]string, 0, len(c.Series))

	for _, s := range c.Series {
		if s.Last == "" {
			continue
		}

		parts = append(parts, s.Label+": now "+s.Last+", lowest "+s.Min+", highest "+s.Max)
	}

	if len(parts) == 0 {
		return c.Title + ": no data."
	}

	return c.Title + ". " + strings.Join(parts, "; ") + "."
}

// loadRows keeps one sample per tableStep, newest first.
func loadRows(samples []app.LoadSample) []loadRow {
	var (
		out  []loadRow
		next time.Time
	)

	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if !next.IsZero() && s.At.After(next) {
			continue
		}

		next = s.At.Add(-tableStep)

		r := loadRow{At: clock(s.At) + " UTC", CPU: pct(cpuPercent(s.CPU)), Load: num(s.Load1, 2), Mem: "—", Temp: "—"}
		if m, ok := memUsed(s); ok {
			r.Mem = pct(m)
		}

		if s.TempC != nil {
			r.Temp = num(*s.TempC, 1) + " °C"
		}

		out = append(out, r)
	}

	return out
}
