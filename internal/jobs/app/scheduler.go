// Package app holds the periodic jobs of the hub: the scheduler that runs
// registered jobs without overlap and records their runs, and the
// retention view (ADM-011, TECHNICAL_SPEC §7.3, ADR 0010). Ports are
// declared here; adapters live in internal/jobs/infra and in the modules
// that own the data.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/jobs/domain"
)

// StaleAfter is how long a run may last before it is considered dead (the
// process stopped during the run) and another run may start.
const StaleAfter = time.Hour

// Job is a periodic unit of work. Run deletes or updates in bounded batches
// and returns the rows affected.
type Job interface {
	Name() string
	Run(ctx context.Context) (int64, error)
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Clock returns the current time.
type Clock func() time.Time

type scheduled struct {
	name  domain.Name
	job   Job
	every time.Duration
}

// Scheduler runs registered jobs at start then every period, never two runs
// of one job at once (across processes too, through job_runs). It is the
// boundary of the jobs: it logs their outcome once.
type Scheduler struct {
	repo   domain.Repository
	tx     Transactor
	now    Clock
	logger *slog.Logger

	mu   sync.Mutex
	jobs map[string]scheduled
}

// NewScheduler returns a scheduler without jobs.
func NewScheduler(repo domain.Repository, tx Transactor, now Clock, logger *slog.Logger) *Scheduler {
	return &Scheduler{repo: repo, tx: tx, now: now, logger: logger, jobs: map[string]scheduled{}}
}

// Register adds a job run every period. It panics on an invalid or
// duplicate name (a wiring defect).
func (s *Scheduler) Register(job Job, every time.Duration) {
	name := domain.MustName(job.Name())

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.jobs[name.String()]; dup || every <= 0 {
		panic(fmt.Sprintf("jobs: invalid registration of %s", name))
	}

	s.jobs[name.String()] = scheduled{name: name, job: job, every: every}
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

	var wg sync.WaitGroup

	for _, j := range jobs {
		wg.Go(func() {
			t := time.NewTicker(j.every)
			defer t.Stop()

			for {
				_, err := s.run(ctx, j)
				if err != nil && !errors.Is(err, domain.ErrJobRunning) && ctx.Err() == nil {
					s.logger.ErrorContext(ctx, "job failed", slog.String("job", j.name.String()), slog.Any("error", err))
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
// It returns domain.ErrUnknownJob, or domain.ErrJobRunning when the job is
// already running.
func (s *Scheduler) RunNow(ctx context.Context, name string) (int64, error) {
	s.mu.Lock()
	j, ok := s.jobs[name]
	s.mu.Unlock()

	if !ok {
		return 0, domain.ErrUnknownJob.WithDetail("unknown job " + name)
	}

	return s.run(ctx, j)
}

// LastRun returns the bookkeeping of a job (a run that never happened when
// the job never ran).
func (s *Scheduler) LastRun(ctx context.Context, name string) (*domain.Run, error) {
	n, err := domain.NewName(name)
	if err != nil {
		return nil, err
	}

	r, err := s.repo.Get(ctx, n)
	if err != nil {
		return nil, err
	}

	if r == nil {
		r = domain.NewRun(n)
	}

	return r, nil
}

// run takes the job's run atomically, runs it and records the outcome.
func (s *Scheduler) run(ctx context.Context, j scheduled) (int64, error) {
	var run *domain.Run

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.Get(ctx, j.name)
		if err != nil {
			return err
		}

		if r == nil {
			r = domain.NewRun(j.name)
		}

		if err := r.Start(s.now(), StaleAfter); err != nil {
			return err
		}

		run = r

		return s.repo.Save(ctx, r)
	})
	if errors.Is(err, domain.ErrJobRunning) {
		s.logger.DebugContext(ctx, "job already running, skipped", slog.String("job", j.name.String()))

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

	s.logger.LogAttrs(ctx, level, "job done", slog.String("job", j.name.String()),
		slog.Int64("rows", rows), slog.Duration("duration", s.now().Sub(start)))

	return rows, nil
}

// Batched calls fn with batch until it affects fewer rows, and returns the
// total: deletes in bounded batches keep each write transaction short
// (TECHNICAL_SPEC §7.3: at most 10 000 rows per transaction).
func Batched(ctx context.Context, batch int, fn func(ctx context.Context, batch int) (int, error)) (int64, error) {
	var total int64

	for {
		n, err := fn(ctx, batch)
		total += int64(n)

		if err != nil || n < batch {
			return total, err
		}

		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
