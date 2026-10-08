package decodes

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// JS8 threads (DEC-030): a JS8Call message longer than one frame is sent
// as several frames on one audio frequency; the first frame has bit 1 of
// its thread type, the last one bit 2.
const (
	js8Schema = "js8.v1"
	// threadSpan is the frequency difference (Hz) of the frames of one
	// thread.
	threadSpan = 5
	// threadGap ends a thread whose next frame does not come.
	threadGap = 5 * time.Minute
)

// js8Frame is what a thread needs of a js8.v1 payload.
type js8Frame struct {
	Submode    string `json:"submode"`
	ThreadType int    `json:"thread_type"`
	Callsign   string `json:"callsign"`
	To         string `json:"to"`
}

// Entry is one line of the Decodes table: a message, or the frames of a
// JS8 thread joined (DEC-030).
type Entry struct {
	Message
	// Calls are the call signs of a JS8 thread.
	Calls []string
	// Open: the last frame of the thread has not come (a continuation
	// marker is shown).
	Open bool
	// Frames counts the frames of a JS8 thread (0 for other messages).
	Frames int
}

type thread struct {
	entry *Entry
	frame js8Frame
	last  time.Time
	// freq is the frequency of the last frame (Hz).
	freq int64
}

// Threads groups the JS8 messages of rows (newest first) into threads by
// node, device, submode and frequency (±5 Hz) between a first and a last
// frame; other messages stay as they are. The result is newest first, a
// thread at the place of its newest frame. Text stays plain: templ
// escapes it.
func Threads(rows []Message) []Entry {
	var (
		order []*Entry
		open  []*thread
	)

	for _, m := range slices.Backward(rows) {
		var f js8Frame

		if m.Schema != js8Schema || json.Unmarshal(m.Payload, &f) != nil {
			order = append(order, &Entry{Message: m})

			continue
		}

		var t *thread

		if f.ThreadType&1 == 0 {
			for _, o := range open {
				if o.entry.NodeID == m.NodeID && o.entry.DeviceID == m.DeviceID && o.frame.Submode == f.Submode &&
					abs(o.freq-m.FreqHz) <= threadSpan && m.DecodedAt.Sub(o.last) <= threadGap {
					t = o
				}
			}
		}

		if t == nil {
			t = &thread{entry: &Entry{Message: m, Open: true}, frame: f}
			open = append(open, t)
		} else {
			t.entry.Text += m.Text
			t.entry.ID = m.ID
			// The thread moves to its newest frame.
			order = slices.DeleteFunc(order, func(e *Entry) bool { return e == t.entry })
		}

		order = append(order, t.entry)
		t.entry.Frames++
		t.last, t.freq = m.DecodedAt, m.FreqHz

		for _, c := range []string{f.Callsign, f.To} {
			if c != "" && !slices.Contains(t.entry.Calls, c) {
				t.entry.Calls = append(t.entry.Calls, c)
			}
		}

		if f.ThreadType&2 != 0 {
			t.entry.Open = false
			open = slices.DeleteFunc(open, func(o *thread) bool { return o == t })
		}
	}

	out := make([]Entry, 0, len(order))
	for _, e := range slices.Backward(order) {
		e.Text = strings.TrimRight(e.Text, " ")
		out = append(out, *e)
	}

	return out
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}
