package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/reporting/app"
	"github.com/yohang/mesh-sdr/internal/reporting/domain"
	"github.com/yohang/mesh-sdr/internal/reporting/infra/sqlite"
)

// transport records the batches and fails the entries listed in fail.
type transport struct {
	enabled bool
	fail    map[int64]bool

	mu      sync.Mutex
	batches [][]int64
}

func (*transport) Network() domain.Network { return domain.NetworkMQTT }
func (t *transport) Enabled() bool         { return t.enabled }
func (*transport) BatchSize() int          { return 2 }

func (t *transport) Send(_ context.Context, batch []*domain.Entry) []error {
	t.mu.Lock()
	defer t.mu.Unlock()

	ids := make([]int64, len(batch))
	errs := make([]error, len(batch))

	for i, e := range batch {
		ids[i] = e.ID()
		if t.fail[e.ID()] {
			errs[i] = errors.New("broker refused")
		}
	}

	t.batches = append(t.batches, ids)

	return errs
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type durations map[string]time.Duration

func (d durations) Duration(key string) time.Duration { return d[key] }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func enqueue(t *testing.T, e *app.Engine, network domain.Network, key byte, now time.Time) {
	t.Helper()

	entry, err := domain.NewEntry(network, []byte(`{"call":"F4XYZ"}`), domain.DedupKey{key}, nil, now)
	if err != nil {
		t.Fatal(err)
	}

	if added, err := e.Enqueue(context.Background(), entry); err != nil || !added {
		t.Fatalf("enqueue = %v, %v", added, err)
	}
}

func counts(t *testing.T, e *app.Engine) map[domain.Status]int64 {
	t.Helper()

	st, err := e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range st {
		if n.Network == domain.NetworkMQTT {
			return n.Counts
		}
	}

	return nil
}

func TestDrain(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	repo := sqlite.NewOutbox(dbtest.NewSQLite(t))
	tr := &transport{enabled: true, fail: map[int64]bool{2: true}}
	e := app.NewEngine(repo, []app.Transport{tr}, app.Options{Owner: "test"}, c.now, discard())

	for k := byte(1); k <= 3; k++ {
		enqueue(t, e, domain.NetworkMQTT, k, c.t)
	}

	// The same dedup key is not enqueued twice.
	dup, _ := domain.NewEntry(domain.NetworkMQTT, []byte(`{}`), domain.DedupKey{1}, nil, c.t)
	if added, err := e.Enqueue(ctx, dup); err != nil || added {
		t.Errorf("duplicate = %v, %v", added, err)
	}

	sent, err := e.Drain(ctx, tr)
	if err != nil || sent != 2 {
		t.Fatalf("drain = %d, %v", sent, err)
	}

	if len(tr.batches) != 2 || len(tr.batches[0]) != 2 {
		t.Errorf("batches = %v", tr.batches)
	}

	if got := counts(t, e); got[domain.StatusSent] != 2 || got[domain.StatusFailed] != 1 {
		t.Errorf("counts = %v", got)
	}

	// The failed entry is retried after its back-off.
	if sent, _ := e.Drain(ctx, tr); sent != 0 || len(tr.batches) != 2 {
		t.Errorf("retried too early: %v", tr.batches)
	}

	c.t = c.t.Add(31 * time.Second)
	tr.fail = nil

	if sent, err := e.Drain(ctx, tr); err != nil || sent != 1 {
		t.Errorf("retry = %d, %v", sent, err)
	}

	st, _ := e.Status(ctx)
	for _, n := range st {
		if n.Network == domain.NetworkMQTT && (!n.Enabled || n.LastSentAt.IsZero() || !n.OldestDueAt.IsZero()) {
			t.Errorf("status = %+v", n)
		}
	}
}

func TestLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	repo := sqlite.NewOutbox(dbtest.NewSQLite(t))
	e := app.NewEngine(repo, nil, app.Options{}, c.now, discard())

	enqueue(t, e, domain.NetworkMQTT, 1, c.t)

	// A worker that crashed after its claim.
	claimed, err := repo.Claim(ctx, domain.NetworkMQTT, "crashed", c.t, c.t.Add(time.Minute), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v", claimed, err)
	}

	if again, _ := repo.Claim(ctx, domain.NetworkMQTT, "other", c.t, c.t.Add(time.Minute), 10); len(again) != 0 {
		t.Errorf("claimed twice: %v", again)
	}

	// Its late save is ignored once another worker took the entry over.
	later := c.t.Add(2 * time.Minute)

	again, err := repo.Claim(ctx, domain.NetworkMQTT, "other", later, later.Add(time.Minute), 10)
	if err != nil || len(again) != 1 {
		t.Fatalf("reclaim = %v, %v", again, err)
	}

	claimed[0].Sent(later)

	if ok, err := repo.Save(ctx, claimed[0], "crashed"); err != nil || ok {
		t.Errorf("stale save = %v, %v", ok, err)
	}
}

func TestOverflowAndPurge(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	repo := sqlite.NewOutbox(dbtest.NewSQLite(t))
	tr := &transport{enabled: true}
	e := app.NewEngine(repo, []app.Transport{tr}, app.Options{MaxPending: 2}, c.now, discard())

	for k := byte(1); k <= 3; k++ {
		enqueue(t, e, domain.NetworkMQTT, k, c.t)
	}

	if got := counts(t, e); got[domain.StatusPending] != 2 || got[domain.StatusDead] != 1 {
		t.Errorf("after overflow = %v", got)
	}

	// Entries of a network without an enabled transport die after the
	// pending TTL.
	enqueue(t, e, domain.NetworkAPRSIS, 9, c.t)
	enqueue(t, e, domain.NetworkAPRSIS, 10, c.t)

	// A worker of that network claimed one entry and crashed: its expired
	// lease counts as waiting.
	if got, err := repo.Claim(ctx, domain.NetworkAPRSIS, "crashed", c.t, c.t.Add(time.Minute), 1); err != nil || len(got) != 1 {
		t.Fatalf("claim = %v, %v", got, err)
	}

	if _, err := e.Drain(ctx, tr); err != nil {
		t.Fatal(err)
	}

	job := app.NewPurgeJob(repo, e, durations{app.KeySentRetention: 7 * 24 * time.Hour, app.KeyDeadRetention: 30 * 24 * time.Hour}, c.now)
	if job.Name() != app.JobOutboxPurge {
		t.Errorf("name = %s", job.Name())
	}

	if n, err := job.Run(ctx); err != nil || n != 0 {
		t.Errorf("early purge = %d, %v", n, err)
	}

	c.t = c.t.Add(2 * time.Hour)

	if n, err := job.Run(ctx); err != nil || n != 2 {
		t.Errorf("pending TTL = %d, %v", n, err)
	}

	c.t = c.t.Add(8 * 24 * time.Hour)

	if n, err := job.Run(ctx); err != nil || n != 2 {
		t.Errorf("sent purge = %d, %v", n, err)
	}

	c.t = c.t.Add(30 * 24 * time.Hour)

	if n, err := job.Run(ctx); err != nil || n != 3 {
		t.Errorf("dead purge = %d, %v", n, err)
	}
}
