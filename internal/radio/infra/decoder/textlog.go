package decoder

import (
	"bytes"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The text log of a skimmer session (FIL-005): the characters of each
// signal frequency are joined into lines, like the lines of the text
// decoders (a line ends at a line break, at maxLine characters or after
// lineIdle without a new character), written "<UTC time> <frequency Hz>
// <text>". The log is saved into Files as a text_log file when the session
// ends, when it reaches textLogBytes or when it covers textLogPeriod,
// whichever comes first. An empty log is not saved.
const (
	textLogBytes  = 1 << 20
	textLogPeriod = time.Hour
)

// logLine is the line being assembled for one frequency.
type logLine struct {
	text  []byte
	start time.Time
	last  time.Time
	// dial is the dial frequency when the line started.
	dial int64
}

// textLog is the text log of one session.
type textLog struct {
	mu    sync.Mutex
	lines map[int64]*logLine
	buf   bytes.Buffer
	start time.Time
	end   time.Time
	// dial is the dial frequency of the first line of the log (the file's
	// reception frequency, FIL-008).
	dial int64
}

// add appends the characters of rec at frequency dial+rec.AudioHz,
// received at now; it returns the log to save when it is full.
func (l *textLog) add(rec app.DecodeRecord, dial int64, now time.Time) *app.ProducedFile {
	if dial <= 0 {
		return nil
	}

	freq := dial + rec.AudioHz

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lines == nil {
		l.lines = map[int64]*logLine{}
	}

	ln := l.lines[freq]
	if ln == nil {
		ln = &logLine{}
		l.lines[freq] = ln
	}

	for _, r := range rec.Text {
		if r == '\n' || r == '\r' {
			l.endLine(freq, ln)

			continue
		}

		if len(ln.text) == 0 {
			if r == ' ' {
				continue
			}

			ln.start, ln.dial = rec.Time, dial
		}

		ln.text = append(ln.text, string(r)...)

		if len(ln.text) >= maxLine {
			l.endLine(freq, ln)
		}
	}

	ln.last = now

	return l.full()
}

// tick ends the lines idle for lineIdle at now; it returns the log to
// save when it covers textLogPeriod.
func (l *textLog) tick(now time.Time) *app.ProducedFile {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, freq := range slices.Sorted(maps.Keys(l.lines)) {
		if ln := l.lines[freq]; len(ln.text) > 0 && now.Sub(ln.last) >= lineIdle {
			l.endLine(freq, ln)
		}
	}

	if l.buf.Len() > 0 && now.Sub(l.start) >= textLogPeriod {
		return l.take()
	}

	return l.full()
}

// flush ends every line and returns what the log holds (the session
// ended at now), nil when empty.
func (l *textLog) flush(now time.Time) *app.ProducedFile {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, freq := range slices.Sorted(maps.Keys(l.lines)) {
		l.endLine(freq, l.lines[freq])
	}

	if l.buf.Len() > 0 && now.After(l.end) {
		l.end = now
	}

	return l.take()
}

// endLine writes the line of freq, if any, to the log (l.mu held).
func (l *textLog) endLine(freq int64, ln *logLine) {
	text := strings.TrimRight(string(ln.text), " ")
	ln.text = ln.text[:0]

	if text == "" {
		return
	}

	if l.buf.Len() == 0 {
		l.start, l.dial = ln.start, ln.dial
	}

	l.buf.WriteString(ln.start.UTC().Format(time.RFC3339) + " " + strconv.FormatInt(freq, 10) + " " + text + "\n")
	l.end = ln.start
}

// full returns the log when it reached textLogBytes or covers
// textLogPeriod (l.mu held).
func (l *textLog) full() *app.ProducedFile {
	if l.buf.Len() >= textLogBytes || (l.buf.Len() > 0 && l.end.Sub(l.start) >= textLogPeriod) {
		return l.take()
	}

	return nil
}

// take empties the log into a file (l.mu held).
func (l *textLog) take() *app.ProducedFile {
	if l.buf.Len() == 0 {
		return nil
	}

	f := &app.ProducedFile{Kind: app.FileTextLog, Data: bytes.Clone(l.buf.Bytes()), Start: l.start, End: l.end, FreqHz: l.dial}
	l.buf.Reset()
	l.dial = 0

	return f
}
