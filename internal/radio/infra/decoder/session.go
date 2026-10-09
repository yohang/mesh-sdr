package decoder

import (
	"log/slog"
	"sync"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// sessionBase is what every decoder session shares: its events, its logger
// and its closed state. Nothing is reported to the listener after Close
// and a status is reported only when it changes. The input methods of
// app.DecoderRun are no-ops: each session implements the input it reads.
type sessionBase struct {
	ev  app.DecoderEvents
	log *slog.Logger

	mu     sync.Mutex
	closed bool
	last   app.DecoderStatus
	// convFailing: the last audio block was not converted (logged once).
	convFailing bool
}

// sessionLog is the logger of a session.
func (r *Runner) sessionLog(spec app.DecoderSpec) *slog.Logger {
	return r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name))
}

// status reports a status change unless the session is closed.
func (b *sessionBase) status(st app.DecoderStatus) {
	b.mu.Lock()
	changed := !b.closed && st != b.last
	if changed {
		b.last = st
	}
	b.mu.Unlock()

	if changed {
		b.ev.Status(st)
	}
}

// decode reports a record, its text capped (§8.4 rule 4), unless the
// session is closed.
func (b *sessionBase) decode(rec app.DecodeRecord) {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()

	if closed || b.ev.Decode == nil {
		return
	}

	rec.Text = shared.Truncate(rec.Text, maxText)
	b.ev.Decode(rec)
}

// convert converts an audio block for a tool (b.mu held); a failure is
// logged once, until a block converts again.
func (b *sessionBase) convert(c *dsp.S16Converter, a app.AudioBlock) []byte {
	out, err := c.Convert(a.Samples, a.Rate)
	if err != nil {
		if !b.convFailing {
			b.log.Warn("decoder input not converted", slog.Int("rate", a.Rate), slog.Any("error", err))
		}

		b.convFailing = true

		return nil
	}

	b.convFailing = false

	return out
}

// Audio implements app.DecoderRun.
func (b *sessionBase) Audio(app.AudioBlock) {}

// IQ implements app.DecoderRun.
func (b *sessionBase) IQ(app.IQBlock) {}

// WideIQ implements app.DecoderRun.
func (b *sessionBase) WideIQ(app.WideIQBlock) {}

// Retune implements app.DecoderRun: no secondary selector.
func (b *sessionBase) Retune(float64) {}

// SpectrumSize implements app.DecoderRun: no secondary FFT.
func (b *sessionBase) SpectrumSize() int { return 0 }

// Spectrum implements app.DecoderRun: no secondary FFT.
func (b *sessionBase) Spectrum(int) {}

// statusOf maps a supervisor transition to the session status (DEC-002):
// running once a streaming tool reads its input or a batch job completed,
// unavailable when the tool is missing or refuses its configuration, error
// on any other failure. ok is false for a transition that changes nothing.
func statusOf(e process.Event, batch bool) (st app.DecoderStatus, ok bool) {
	switch e.State {
	case process.StateRunning:
		if batch || e.Diag != process.DiagNone {
			return st, false
		}

		return app.DecoderStatus{State: app.DecoderRunning}, true
	case process.StateStopped:
		if !batch || e.Reason != "completed" {
			return st, false
		}

		return app.DecoderStatus{State: app.DecoderRunning}, true
	case process.StateUnavailable:
		return app.DecoderStatus{State: app.DecoderUnavailable, Reason: e.Reason}, true
	case process.StateErrored, process.StateTimedOut:
		return app.DecoderStatus{State: app.DecoderError, Reason: e.Reason}, true
	case process.StateRetryWait, process.StateCrashLoop, process.StateFailed:
		if batch {
			return st, false
		}

		return app.DecoderStatus{State: app.DecoderError, Reason: e.Reason}, true
	default:
		return st, false
	}
}

// sink reports the supervisor transitions of a session's processes as its
// status; a missing tool re-probes the node (§8.4).
func (r *Runner) sink(batch bool, status func(app.DecoderStatus)) func(process.Event) {
	return func(e process.Event) {
		st, ok := statusOf(e, batch)
		if !ok {
			return
		}

		if e.Reprobe {
			r.o.Reprobe()
		}

		status(st)
	}
}

// dropQueue is the input queue of a session goroutine (§8.3): the DSP tap
// pushes without blocking; beyond limit (in units of weight) the oldest
// items are dropped and the first item left is flagged as following a
// gap.
type dropQueue[T any] struct {
	limit  int
	weight func(T) int
	// wake is signalled on every push and on close.
	wake chan struct{}

	mu      sync.Mutex
	items   []queued[T]
	size    int
	closed  bool
	dropped int64
}

// queued is an item of a dropQueue; gap: input was lost before it.
type queued[T any] struct {
	v   T
	gap bool
}

func newDropQueue[T any](limit int, weight func(T) int) *dropQueue[T] {
	return &dropQueue[T]{limit: limit, weight: weight, wake: make(chan struct{}, 1)}
}

// one weighs every item 1.
func one[T any](T) int { return 1 }

func (q *dropQueue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// push queues v (gap: input was lost before it), unless the queue is
// closed.
func (q *dropQueue[T]) push(v T, gap bool) {
	w := q.weight(v)

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()

		return
	}

	lost := false

	for len(q.items) > 0 && q.size+w > q.limit {
		q.size -= q.weight(q.items[0].v)
		q.items[0] = queued[T]{}
		q.items = q.items[1:]
		q.dropped++
		lost = true
	}

	if lost && len(q.items) > 0 {
		q.items[0].gap, lost = true, false
	}

	q.items = append(q.items, queued[T]{v: v, gap: gap || lost})
	q.size += w
	q.mu.Unlock()

	q.signal()
}

// take returns the queued items and whether the queue is closed.
func (q *dropQueue[T]) take() ([]queued[T], bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	items := q.items
	q.items, q.size = nil, 0

	return items, q.closed
}

// close refuses new items; the queued ones can still be taken.
func (q *dropQueue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()

	q.signal()
}

// discard closes the queue and drops its items.
func (q *dropQueue[T]) discard() {
	q.mu.Lock()
	q.closed, q.items, q.size = true, nil, 0
	q.mu.Unlock()

	q.signal()
}

// len returns the number of items queued.
func (q *dropQueue[T]) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// drops returns the number of items dropped.
func (q *dropQueue[T]) drops() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.dropped
}
