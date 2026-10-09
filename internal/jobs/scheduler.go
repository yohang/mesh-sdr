// Package jobs holds the periodic jobs of the hub (TECHNICAL_SPEC §7.3
// "Retention jobs", §7.1 `job_runs`, ADR 0010): the scheduler that runs
// registered jobs without overlap and records their runs in SQLite, and the
// retention view (ADM-011) with its table statistics. The jobs themselves
// live in the modules that own the data.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
)

// StaleAfter is how long a run recorded by another process may last before
// it is considered dead and another run may start. Within the hub, a job
// never overlaps itself whatever its duration (one lock per job), and the
// runs a previous hub process left in progress are ended at start.
const StaleAfter = time.Hour

// Job is a periodic unit of work. Run deletes or updates in bounded batches
// (db.Batched) and returns the rows affected.
type Job interface {
	Name() string
	Run(ctx context.Context) (int64, error)
}

// Func is a Job made of a name and a function.
func Func(name string, run func(ctx context.Context) (int64, error)) Job {
	return funcJob{name: name, run: run}
}

type funcJob struct {
	name string
	run  func(ctx context.Context) (int64, error)
}

func (j funcJob) Name() string                           { return j.name }
func (j funcJob) Run(ctx context.Context) (int64, error) { return j.run(ctx) }

// Clock returns the current time.
type Clock func() time.Time

type scheduled struct {
	name  string
	job   Job
	every time.Duration
	lock  *sync.Mutex
}

// Scheduler runs registered jobs at start then every period, never two runs
// of one job at once (across processes too, through job_runs). It is the
// boundary of the jobs: it logs their outcome once.
type Scheduler struct {
	repo   *Runs
	tx     *db.DB
	now    Clock
	logger *slog.Logger

	mu   sync.Mutex
	jobs map[string]scheduled
}

// NewScheduler returns a scheduler without jobs.
func NewScheduler(repo *Runs, tx *db.DB, now Clock, logger *slog.Logger) *Scheduler {
	return &Scheduler{repo: repo, tx: tx, now: now, logger: logger, jobs: map[string]scheduled{}}
}

// Register adds a job run every period. It panics on an invalid or
// duplicate name (a wiring defect).
func (s *Scheduler) Register(job Job, every time.Duration) {
	name := job.Name()

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.jobs[name]; dup || every <= 0 || !validJobName(name) {
		panic(fmt.Sprintf("jobs: invalid registration of %s", name))
	}

	s.jobs[name] = scheduled{name: name, job: job, every: every, lock: &sync.Mutex{}}
}

// Run runs every job at start, then on its period, until ctx is done. It is
// a background worker.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	jobs := make([]scheduled, 0, len(s.jobs))

	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()

	for _, j := range jobs {
		if err := s.abandon(ctx, j.name); err != nil {
			s.logger.ErrorContext(ctx, "end interrupted job run", slog.String("job", j.name), slog.Any("error", err))
		}
	}

	var wg sync.WaitGroup

	for _, j := range jobs {
		wg.Go(func() {
			t := time.NewTicker(j.every)
			defer t.Stop()

			for {
				_, err := s.run(ctx, j)
				if err != nil && !errors.Is(err, ErrJobRunning) && ctx.Err() == nil {
					s.logger.ErrorContext(ctx, "job failed", slog.String("job", j.name), slog.Any("error", err))
				}

				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		})
	}

	wg.Wait()
}

// RunNow runs a job at once ("purge now") and returns the rows affected.
// It returns ErrUnknownJob, or ErrJobRunning when the job is
// already running.
func (s *Scheduler) RunNow(ctx context.Context, name string) (int64, error) {
	s.mu.Lock()
	j, ok := s.jobs[name]
	s.mu.Unlock()

	if !ok {
		return 0, ErrUnknownJob.WithDetail("unknown job " + name)
	}

	return s.run(ctx, j)
}

// LastRun returns the bookkeeping of a job (a run that never happened when
// the job never ran).
func (s *Scheduler) LastRun(ctx context.Context, name string) (*Run, error) {
	if !validJobName(name) {
		return nil, ErrInvalidJobName.WithDetail("invalid job name " + strconv.Quote(name))
	}

	r, err := s.repo.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	if r == nil {
		r = NewRun(name)
	}

	return r, nil
}

// abandon ends the run of a job left in progress by a previous hub process
// (the hub is the only process running jobs).
func (s *Scheduler) abandon(ctx context.Context, name string) error {
	return s.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.Get(ctx, name)
		if err != nil || r == nil || !r.Abandon(s.now()) {
			return err
		}

		s.logger.WarnContext(ctx, "job run interrupted by a hub stop", slog.String("job", name))

		return s.repo.Save(ctx, r)
	})
}

// run takes the job's run atomically, runs it and records the outcome.
func (s *Scheduler) run(ctx context.Context, j scheduled) (int64, error) {
	if !j.lock.TryLock() {
		s.logger.DebugContext(ctx, "job already running, skipped", slog.String("job", j.name))

		return 0, ErrJobRunning.WithDetail("job " + j.name + " is already running")
	}
	defer j.lock.Unlock()

	var run *Run

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.Get(ctx, j.name)
		if err != nil {
			return err
		}

		if r == nil {
			r = NewRun(j.name)
		}

		if err := r.Start(s.now(), StaleAfter); err != nil {
			return err
		}

		run = r

		return s.repo.Save(ctx, r)
	})
	if errors.Is(err, ErrJobRunning) {
		s.logger.DebugContext(ctx, "job already running, skipped", slog.String("job", j.name))

		return 0, err
	}

	if err != nil {
		return 0, fmt.Errorf("start job %s: %w", j.name, err)
	}

	start := s.now()
	rows, runErr := j.job.Run(ctx)

	run.Finish(s.now(), rows, runErr)

	// Record the outcome even when ctx is done (shutdown during a run).
	if err := s.repo.Save(context.WithoutCancel(ctx), run); err != nil {
		return rows, errors.Join(runErr, fmt.Errorf("record job %s: %w", j.name, err))
	}

	if runErr != nil {
		return rows, fmt.Errorf("job %s: %w", j.name, runErr)
	}

	level := slog.LevelDebug
	if rows > 0 {
		level = slog.LevelInfo
	}

	s.logger.LogAttrs(ctx, level, "job done", slog.String("job", j.name),
		slog.Int64("rows", rows), slog.Duration("duration", s.now().Sub(start)))

	return rows, nil
}
