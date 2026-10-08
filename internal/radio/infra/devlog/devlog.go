// Package devlog keeps the device logs of a node in RAM (SRC-005): the
// last records of each device, the lines its connector writes to stderr
// and its lifecycle (state changes). These are ephemeral diagnostics, not
// state: nothing is written to disk. The control channel sends them to the
// hub as device.log messages: the records held when it opens (the backlog),
// then the new ones.
package devlog

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

// DefaultSize is the number of records kept per device (SRC-005).
const DefaultSize = 200

// maxMessageBytes bounds the records of one device.log message, well
// under the 64 KiB control message limit (a record is at most
// ctl.MaxLogText bytes of text, up to six times longer once JSON-escaped).
const maxMessageBytes = 32 << 10

// entry is one record and its number in the node log.
type entry struct {
	id  uint64
	rec ctl.LogRecord
}

type device struct {
	ring  *process.Ring[entry]
	state string
}

// Log holds the device logs. Safe for concurrent use.
type Log struct {
	size int
	now  func() time.Time

	mu      sync.Mutex
	ids     []string
	devices map[string]*device
	last    uint64
	changed chan struct{}
}

// New returns the log of the devices ids, size records each (DefaultSize
// when size ≤ 0). Records of other devices are ignored.
func New(ids []string, size int, now func() time.Time) *Log {
	if size <= 0 {
		size = DefaultSize
	}

	l := &Log{size: size, now: now, ids: slices.Sorted(slices.Values(ids)), devices: make(map[string]*device, len(ids)),
		changed: make(chan struct{})}
	for _, id := range ids {
		l.devices[id] = &device{ring: process.NewRing[entry](size)}
	}

	return l
}

// Connector records a stderr line of the connector of a device.
func (l *Log) Connector(id string, line process.Line) {
	text := line.Text
	if line.Truncated {
		text += " …"
	}

	l.add(id, ctl.LogRecord{Time: line.Time.UnixMilli(), Source: ctl.LogSourceConnector, Class: string(line.Class), Text: text})
}

// State records a lifecycle change of a device: a state, or a reason,
// different from the last recorded one.
func (l *Log) State(id, state, reason string) {
	text := "state " + state
	if reason != "" {
		text += " (" + reason + ")"
	}

	l.mu.Lock()
	d, ok := l.devices[id]
	if !ok || d.state == text {
		l.mu.Unlock()

		return
	}

	d.state = text
	l.mu.Unlock()

	l.add(id, ctl.LogRecord{Time: l.now().UnixMilli(), Source: ctl.LogSourceDevice, Class: state, Text: text})
}

func (l *Log) add(id string, rec ctl.LogRecord) {
	rec.Text = capText(rec.Text)

	l.mu.Lock()
	defer l.mu.Unlock()

	d, ok := l.devices[id]
	if !ok {
		return
	}

	l.last++
	d.ring.Add(entry{id: l.last, rec: rec})

	close(l.changed)
	l.changed = make(chan struct{})
}

// capText cuts text to ctl.MaxLogText bytes, on a rune boundary.
func capText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= ctl.MaxLogText {
		return s
	}

	cut := ctl.MaxLogText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}

// Changed returns a channel closed at the next record.
func (l *Log) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.changed
}

// Records returns the records of a device, oldest first.
func (l *Log) Records(id string) []ctl.LogRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	d, ok := l.devices[id]
	if !ok {
		return nil
	}

	entries := d.ring.Tail(l.size)
	out := make([]ctl.LogRecord, 0, len(entries))

	for _, e := range entries {
		out = append(out, e.rec)
	}

	return out
}

// Backlog returns the records held, as device.log messages: every device
// gets at least one, the first of each has Reset set (a device without
// records clears the hub's copy). cursor is the number of the last record,
// for Since.
func (l *Log) Backlog() (msgs []ctl.DeviceLog, cursor uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, id := range l.ids {
		msgs = append(msgs, chunk(id, true, l.devices[id].ring.Tail(l.size))...)
	}

	return msgs, l.last
}

// Since returns the records numbered after cursor, as device.log messages
// to append, and the new cursor. Records evicted meanwhile are lost.
func (l *Log) Since(cursor uint64) (msgs []ctl.DeviceLog, next uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if cursor >= l.last {
		return nil, l.last
	}

	for _, id := range l.ids {
		entries := l.devices[id].ring.Tail(l.size)
		i := 0

		for i < len(entries) && entries[i].id <= cursor {
			i++
		}

		if i < len(entries) {
			msgs = append(msgs, chunk(id, false, entries[i:])...)
		}
	}

	return msgs, l.last
}

// chunk splits the records of a device into messages of at most
// maxMessageBytes of records; reset is set on the first one only.
func chunk(id string, reset bool, entries []entry) []ctl.DeviceLog {
	cur := ctl.DeviceLog{DeviceID: id, Reset: reset, Records: []ctl.LogRecord{}}
	out := []ctl.DeviceLog{}
	size := 0

	for _, e := range entries {
		raw, _ := json.Marshal(e.rec)

		if size > 0 && size+len(raw) > maxMessageBytes {
			out = append(out, cur)
			cur = ctl.DeviceLog{DeviceID: id, Records: []ctl.LogRecord{}}
			size = 0
		}

		cur.Records = append(cur.Records, e.rec)
		size += len(raw) + 1
	}

	return append(out, cur)
}
