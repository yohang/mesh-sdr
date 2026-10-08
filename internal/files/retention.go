package files

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// JobRetention is the periodic job of the files retention (FIL-004).
const JobRetention = "files.retention"

// RetentionEvery is the period of the job: it also deletes the files left
// incomplete for IncompleteAfter.
const RetentionEvery = 5 * time.Minute

// RetentionPolicy are the limits of the files the nodes send (FIL-004):
// the newest Count files of each kind are kept; MaxAge and MaxBytes, when
// positive, also delete the files older than MaxAge and the oldest files
// while all of them take more than MaxBytes.
type RetentionPolicy struct {
	Count    int
	MaxAge   time.Duration
	MaxBytes int64
}

// String describes the policy for people.
func (p RetentionPolicy) String() string {
	s := strconv.Itoa(p.Count) + " per kind"

	if p.MaxAge > 0 {
		s += ", " + strconv.FormatInt(int64(p.MaxAge/(24*time.Hour)), 10) + "d"
	}

	if p.MaxBytes > 0 {
		s += ", " + HumanSize(p.MaxBytes)
	}

	return s
}

// Retention applies the retention of the files the nodes send: after each
// new file and every RetentionEvery (it is the job), oldest files first.
// The receiver images are not covered.
type Retention struct {
	db     *db.DB
	policy func() RetentionPolicy
	now    func() time.Time

	mu sync.Mutex
}

// NewRetention returns the retention; policy reads the current settings.
func NewRetention(a *db.DB, policy func() RetentionPolicy, now func() time.Time) *Retention {
	return &Retention{db: a, policy: policy, now: now}
}

// Name implements jobs.Job.
func (r *Retention) Name() string { return JobRetention }

// Run implements jobs.Job: the files incomplete for IncompleteAfter go
// with their chunks, then the retention applies.
func (r *Retention) Run(ctx context.Context) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).PurgeIncompleteFiles(ctx, r.now().Add(-IncompleteAfter).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge incomplete files: %w", err)
	}

	m, err := r.Apply(ctx)

	return n + m, err
}

// Apply deletes the files beyond the policy and returns how many.
func (r *Retention) Apply(ctx context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	p := r.policy()

	var total int64

	err := r.db.WithinTx(ctx, func(ctx context.Context) error {
		total = 0
		q := sqlc.New(r.db.Writer(ctx))

		if p.MaxAge > 0 {
			n, err := q.DeleteProducedFilesBefore(ctx, r.now().Add(-p.MaxAge).UnixMilli())
			if err != nil {
				return fmt.Errorf("delete files older than %s: %w", p.MaxAge, err)
			}

			total += n
		}

		if p.Count > 0 {
			for _, k := range ProducedKinds {
				n, err := q.DeleteFilesBeyondCount(ctx, sqlc.DeleteFilesBeyondCountParams{Kind: string(k), Keep: int64(p.Count)})
				if err != nil {
					return fmt.Errorf("keep %d %s files: %w", p.Count, k, err)
				}

				total += n
			}
		}

		if p.MaxBytes > 0 {
			n, err := r.capSize(ctx, q, p.MaxBytes)
			if err != nil {
				return err
			}

			total += n
		}

		return nil
	})

	return total, err
}

// capSize deletes the oldest files while all of them take more than max
// bytes.
func (r *Retention) capSize(ctx context.Context, q *sqlc.Queries, maxBytes int64) (int64, error) {
	st, err := q.ProducedFilesStats(ctx)
	if err != nil {
		return 0, fmt.Errorf("size of the files: %w", err)
	}

	var n int64

	for st.Bytes > maxBytes {
		oldest, err := q.OldestProducedFiles(ctx, 100)
		if err != nil {
			return n, fmt.Errorf("oldest files: %w", err)
		}

		if len(oldest) == 0 {
			break
		}

		for _, f := range oldest {
			if st.Bytes <= maxBytes {
				break
			}

			if _, err := q.DeleteFile(ctx, f.ID); err != nil {
				return n, fmt.Errorf("delete an old file: %w", err)
			}

			st.Bytes -= f.SizeBytes
			n++
		}
	}

	return n, nil
}

// Stats implements jobs.Stats: the files the nodes sent and their size
// (content only).
func (r *Retention) Stats(ctx context.Context) (rows, bytes int64, sized bool, err error) {
	st, err := sqlc.New(r.db.Reader(ctx)).ProducedFilesStats(ctx)
	if err != nil {
		return 0, 0, false, fmt.Errorf("files stats: %w", err)
	}

	return st.Files, st.Bytes, true, nil
}

// HumanSize formats a size in bytes for people (KiB, MiB, GiB).
func HumanSize(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}

	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTP"[exp]) + "iB"
}
