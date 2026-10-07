package process

import (
	"bufio"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Class is a stderr line class (§8.4 "Stderr and exit-code classification").
type Class string

const (
	ClassInfo        Class = "info"
	ClassSignalInfo  Class = "signal_info"
	ClassWarn        Class = "warn"
	ClassInputError  Class = "input_error"
	ClassFatalConfig Class = "fatal_config"
	ClassResource    Class = "resource"
	ClassUnknown     Class = "unknown"
	// Connector classes (§8.2 rule 5) reuse the same mechanism.
	ClassDeviceLost Class = "device_lost"
	ClassUSBError   Class = "usb_error"
	ClassOverflow   Class = "overflow"
)

// Rule maps a regexp to a class. Rules are evaluated in order; the first match
// wins; no match is ClassUnknown.
type Rule struct {
	Pattern *regexp.Regexp
	Class   Class
}

// Line is one classified stderr line.
type Line struct {
	Time      time.Time
	Class     Class
	Text      string
	Truncated bool
}

const maxLineBytes = 4096

// lineSink is what the line reader feeds.
type lineSink interface{ line(Line) }

// readLines reads r line by line with a bounded line length: longer lines are
// cut at maxLineBytes and the rest is discarded. Text is sanitised (invalid
// UTF-8 replaced, control characters other than \t removed). It returns at EOF.
func readLines(r io.Reader, rules []Rule, sink lineSink) {
	br := bufio.NewReaderSize(r, maxLineBytes)
	skipping := false
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if !skipping {
				sink.line(classify(rules, string(b), true))
			}
			skipping = true
			continue
		}
		if len(b) > 0 && !skipping {
			sink.line(classify(rules, string(b), false))
		}
		skipping = false
		if err != nil {
			return
		}
	}
}

func classify(rules []Rule, raw string, truncated bool) Line {
	text := sanitize(raw)
	c := ClassUnknown
	for _, r := range rules {
		if r.Pattern.MatchString(text) {
			c = r.Class
			break
		}
	}
	return Line{Time: time.Now(), Class: c, Text: text, Truncated: truncated}
}

func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.TrimRight(s, "\r\n")
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// ring keeps the last N lines (served to admins on demand; the last 20 are
// attached to DECODER_ERROR events).
type ring struct {
	mu    sync.Mutex
	lines []Line
	next  int
	full  bool
}

func newRing(n int) *ring { return &ring{lines: make([]Line, n)} }

func (r *ring) add(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return
	}
	r.lines[r.next] = l
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// tail returns up to n most recent lines, oldest first.
func (r *ring) tail(n int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.lines)
	}
	n = min(n, size)
	out := make([]Line, 0, n)
	for i := size - n; i < size; i++ {
		idx := i
		if r.full {
			idx = (r.next + i) % len(r.lines)
		}
		out = append(out, r.lines[idx])
	}
	return out
}

// bucket is a token bucket (stdlib only; golang.org/x/time/rate would do).
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(perSec, burst int) *bucket {
	return &bucket{rate: float64(perSec), burst: float64(burst), tokens: float64(burst)}
}

func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
