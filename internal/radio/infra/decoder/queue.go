package decoder

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Reasons a queued job does not run.
const (
	// ReasonQueueOverflow: the queue was full and the job was the oldest
	// (DEC-025, ADR 0028).
	ReasonQueueOverflow = "queue_overflow"
	// ReasonJobTimeout: no worker took the job before its deadline, or it
	// ran past it (§8.4).
	ReasonJobTimeout = "job_timeout"
	// ReasonStopped: the session closed or the node stops.
	ReasonStopped = "stopped"
)

// Job is one batch decoder job: the decode of one slot by one profile.
type Job struct {
	// Owner tags the job with its session (Purge).
	Owner any
	// Deadline is when the job must have ended (§8.4 job deadlines): a
	// worker gives it the time left, and skips it once it has passed.
	Deadline time.Time
	// Run decodes; budget is the time left before the deadline. ctx ends
	// when the node stops.
	Run func(ctx context.Context, budget time.Duration)
	// Drop is called instead of Run for a job that does not run, with the
	// reason (ReasonQueueOverflow, ReasonJobTimeout).
	Drop func(reason string)
}

// Queue is the batch decoder queue of the node (DEC-025, §8.3 rule 6): the
// jobs of every slot decoder session wait in one bounded FIFO for a fixed
// number of workers (decoders.batch_workers). On overflow the oldest job
// waiting is dropped (decoders.queue_length, ADR 0028).
type Queue struct {
	workers int
	length  int
	now     func() time.Time
	log     *slog.Logger

	wake chan struct{}

	mu   sync.Mutex
	jobs []Job
}

// NewQueue returns a queue of length jobs served by workers workers once
// Run runs.
func NewQueue(workers, length int, now func() time.Time, logger *slog.Logger) *Queue {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	if now == nil {
		now = time.Now
	}

	workers, length = max(1, workers), max(1, length)

	return &Queue{workers: workers, length: length, now: now, log: logger, wake: make(chan struct{}, workers)}
}

// Put queues a job; when the queue is full, the oldest job is dropped
// (ReasonQueueOverflow). It never blocks.
func (q *Queue) Put(j Job) {
	q.mu.Lock()

	var dropped *Job

	if len(q.jobs) >= q.length {
		old := q.jobs[0]
		dropped = &old
		q.jobs = append(q.jobs[:0], q.jobs[1:]...)
	}

	q.jobs = append(q.jobs, j)
	q.mu.Unlock()

	if dropped != nil {
		q.log.Warn("decoder queue full: the oldest job is dropped", slog.Int("queue_length", q.length), slog.Int("workers", q.workers))
		dropped.Drop(ReasonQueueOverflow)
	}

	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Purge drops the waiting jobs of owner (a closed session) with
// ReasonStopped.
func (q *Queue) Purge(owner any) {
	q.mu.Lock()

	var purged []Job

	q.jobs = slices.DeleteFunc(q.jobs, func(j Job) bool {
		if j.Owner == owner {
			purged = append(purged, j)

			return true
		}

		return false
	})
	q.mu.Unlock()

	for _, j := range purged {
		j.Drop(ReasonStopped)
	}
}

// Depth returns the number of jobs waiting for a worker.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.jobs)
}

func (q *Queue) pop() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.jobs) == 0 {
		return Job{}, false
	}

	j := q.jobs[0]
	q.jobs[0] = Job{}
	q.jobs = q.jobs[1:]

	return j, true
}

// Run serves the jobs with the workers until ctx ends (a node worker).
func (q *Queue) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for range q.workers {
		wg.Go(func() { q.work(ctx) })
	}

	wg.Wait()

	// The jobs left go with the node.
	for j, ok := q.pop(); ok; j, ok = q.pop() {
		j.Drop(ReasonStopped)
	}
}

func (q *Queue) work(ctx context.Context) {
	for {
		j, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}

		if ctx.Err() != nil {
			j.Drop(ReasonStopped)

			return
		}

		budget := j.Deadline.Sub(q.now())
		if budget <= 0 {
			q.log.Warn("decoder job skipped: no worker before its deadline", slog.Int("workers", q.workers))
			j.Drop(ReasonJobTimeout)

			continue
		}

		j.Run(ctx, budget)
	}
}
