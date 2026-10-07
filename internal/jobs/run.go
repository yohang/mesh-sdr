package jobs

import (
	"context"
	"regexp"
	"strconv"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Errors of the jobs domain.
var (
	ErrInvalidJobName = shared.NewError(shared.KindInvalid, "invalid_job_name", "invalid job name")
	ErrUnknownJob     = shared.NewError(shared.KindNotFound, "unknown_job", "unknown job")
	ErrJobRunning     = shared.NewError(shared.KindConflict, "job_running", "the job is already running")
	ErrInvalidJobRun  = shared.NewError(shared.KindInvalid, "invalid_job_run", "invalid job run")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// validJobName reports whether s is a job name: a dotted verb such as
// "sessions.reap", at most 64 characters (`job_runs.job`).
func validJobName(s string) bool { return len(s) <= 64 && namePattern.MatchString(s) }

// Status is the outcome of the last finished run.
type Status string

// Statuses.
const (
	StatusNone  Status = "" // never finished
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

// maxErrorLength bounds the recorded error (`job_runs.last_error`).
const maxErrorLength = 512

// Run is the bookkeeping of one job (a `job_runs` row): whether it runs
// now, and the outcome of its last run.
type Run struct {
	name         string
	runningSince time.Time
	lastStarted  time.Time
	lastFinished time.Time
	status       Status
	lastError    string
	rows         int64
}

// NewRun returns the bookkeeping of a job that never ran.
func NewRun(name string) *Run { return &Run{name: name} }

// RunState holds the persisted fields of a run (rehydration).
type RunState struct {
	RunningSince, LastStarted, LastFinished time.Time
	Status                                  Status
	LastError                               string
	Rows                                    int64
}

// RehydrateRun rebuilds a run from its persisted state.
func RehydrateRun(name string, s RunState) (*Run, error) {
	if s.Status != StatusNone && s.Status != StatusOK && s.Status != StatusError {
		return nil, ErrInvalidJobRun.WithDetail("invalid status " + strconv.Quote(string(s.Status)))
	}

	return &Run{
		name: name, runningSince: s.RunningSince, lastStarted: s.LastStarted, lastFinished: s.LastFinished,
		status: s.Status, lastError: s.LastError, rows: s.Rows,
	}, nil
}

// Start marks the run as started at now. A run started less than staleAfter
// ago is still running (ErrJobRunning); an older one is considered dead
// (the process stopped during the run).
func (r *Run) Start(now time.Time, staleAfter time.Duration) error {
	if !r.runningSince.IsZero() && now.Sub(r.runningSince) < staleAfter {
		return ErrJobRunning.WithDetail("job " + r.name + " is already running")
	}

	r.runningSince, r.lastStarted = now.UTC(), now.UTC()

	return nil
}

// Finish records the outcome of the run: rows affected, or the error.
func (r *Run) Finish(now time.Time, rows int64, err error) {
	r.runningSince, r.lastFinished, r.rows = time.Time{}, now.UTC(), rows
	r.status, r.lastError = StatusOK, ""

	if err != nil {
		r.status = StatusError
		r.lastError = truncate(err.Error(), maxErrorLength)
	}
}

// Interrupted is the error recorded for a run that the hub stopped.
const Interrupted = "interrupted: the hub stopped during the run"

// Abandon ends a run left in progress by a stopped process, as failed. It
// reports whether the run was in progress.
func (r *Run) Abandon(now time.Time) bool {
	if r.runningSince.IsZero() {
		return false
	}

	r.runningSince, r.lastFinished, r.status, r.lastError = time.Time{}, now.UTC(), StatusError, Interrupted

	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && s[n]&0xC0 == 0x80 { // do not cut a UTF-8 sequence
		n--
	}

	return s[:n]
}

// Name returns the job name.
func (r *Run) Name() string { return r.name }

// Running reports whether a run is in progress.
func (r *Run) Running() bool { return !r.runningSince.IsZero() }

// RunningSince returns when the current run started (zero when idle).
func (r *Run) RunningSince() time.Time { return r.runningSince }

// LastStarted returns when the last run started (zero: never).
func (r *Run) LastStarted() time.Time { return r.lastStarted }

// LastFinished returns when the last run finished (zero: never).
func (r *Run) LastFinished() time.Time { return r.lastFinished }

// Status returns the outcome of the last finished run.
func (r *Run) Status() Status { return r.status }

// LastError returns the error of the last run, if it failed.
func (r *Run) LastError() string { return r.lastError }

// Rows returns the rows affected by the last finished run.
func (r *Run) Rows() int64 { return r.rows }

// Repository persists runs. Writes join the caller's transaction.
type Repository interface {
	// Get returns the run of name, or nil when the job never ran.
	Get(ctx context.Context, name string) (*Run, error)
	Save(ctx context.Context, r *Run) error
}
