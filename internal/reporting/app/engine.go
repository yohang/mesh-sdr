// Package app holds the reporting engine (TECHNICAL_SPEC §7.3
// "Transactional outbox", §8.6, ADR 0020): one worker per network with a
// transport, which claims due outbox entries with a lease, sends them and
// records the outcome, plus the outbox retention job. Network transports
// (PSKReporter, MQTT…) come with the RPT tickets; ports are declared here.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/reporting/domain"
)

// Clock returns the current time.
type Clock func() time.Time

// Transport delivers the entries of one network.
type Transport interface {
	Network() domain.Network
	// Enabled reads the network's settings on every poll, so a change
	// applies without a restart (§8.6 rule 2).
	Enabled() bool
	// BatchSize is the most entries sent at once.
	BatchSize() int
	// Send delivers a batch; it returns one error per entry (nil when
	// delivered). A transport de-duplicates what the remote side does not
	// (§7.3 rule 3).
	Send(ctx context.Context, batch []*domain.Entry) []error
}

// Options configure the engine. Zero values take the spec defaults.
type Options struct {
	// Owner names the worker in the lease columns (unique per process).
	Owner string
	// Poll is the period of a worker (5 s).
	Poll time.Duration
	// MaxPending caps the pending entries per network (50 000, §7.1).
	MaxPending int
	Policy     domain.Policy
}

// Engine runs one worker per registered transport (one logical worker per
// network, §7.3 rule 2). It is the boundary of the workers: it logs their
// failures.
type Engine struct {
	repo       domain.Repository
	transports map[domain.Network]Transport
	o          Options
	now        Clock
	logger     *slog.Logger
}

// NewEngine returns an engine with the given transports.
func NewEngine(repo domain.Repository, transports []Transport, o Options, now Clock, logger *slog.Logger) *Engine {
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}

	if o.MaxPending == 0 {
		o.MaxPending = 50_000
	}

	if o.Policy == (domain.Policy{}) {
		o.Policy = domain.DefaultPolicy
	}

	if o.Owner == "" {
		o.Owner = "hub"
	}

	m := make(map[domain.Network]Transport, len(transports))
	for _, t := range transports {
		m[t.Network()] = t
	}

	return &Engine{repo: repo, transports: m, o: o, now: now, logger: logger}
}

// Enqueue adds an entry to the outbox (decode ingest calls it in the
// transaction of the decode, §7.3 rule 1) and caps the pending entries of
// its network: beyond the cap the oldest become dead (overflow). It
// reports whether the entry is new.
func (e *Engine) Enqueue(ctx context.Context, entry *domain.Entry) (bool, error) {
	added, err := e.repo.Enqueue(ctx, entry)
	if err != nil || !added {
		return added, err
	}

	n, err := e.repo.KillOverflow(ctx, entry.Network(), e.o.MaxPending)
	if err != nil {
		return true, err
	}

	if n > 0 {
		e.logger.WarnContext(ctx, "reporting outbox full: oldest entries dropped", slog.String("network", string(entry.Network())),
			slog.Int64("dropped", n))
	}

	return true, nil
}

// Enabled reports whether a network has an enabled transport.
func (e *Engine) Enabled(n domain.Network) bool {
	t, ok := e.transports[n]

	return ok && t.Enabled()
}

// Run runs the workers until ctx is done (GRID-001: the hub starts the
// reporting worker). Without transports it only waits.
func (e *Engine) Run(ctx context.Context) {
	e.logger.InfoContext(ctx, "reporting engine started", slog.Int("transports", len(e.transports)))

	var wg sync.WaitGroup

	for _, t := range e.transports {
		wg.Go(func() { e.work(ctx, t) })
	}

	<-ctx.Done()
	wg.Wait()
}

func (e *Engine) work(ctx context.Context, t Transport) {
	tick := time.NewTicker(e.o.Poll)
	defer tick.Stop()

	for {
		if t.Enabled() {
			if _, err := e.Drain(ctx, t); err != nil && ctx.Err() == nil {
				e.logger.ErrorContext(ctx, "reporting worker failed", slog.String("network", string(t.Network())), slog.Any("error", err))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Drain claims and sends the due entries of t's network, one batch at a
// time, until none is due; it returns the entries sent.
func (e *Engine) Drain(ctx context.Context, t Transport) (int, error) {
	owner := e.o.Owner + "/" + string(t.Network())
	sent := 0

	for ctx.Err() == nil {
		now := e.now()

		batch, err := e.repo.Claim(ctx, t.Network(), owner, now, now.Add(e.o.Policy.Lease), max(t.BatchSize(), 1))
		if err != nil {
			return sent, err
		}

		if len(batch) == 0 {
			return sent, nil
		}

		results := t.Send(ctx, batch)
		now = e.now()

		for i, entry := range batch {
			var cause error
			if i < len(results) {
				cause = results[i]
			} else {
				cause = fmt.Errorf("no result from the %s transport", t.Network())
			}

			if cause == nil {
				entry.Sent(now)

				sent++
			} else {
				entry.Failed(cause.Error(), e.o.Policy, now)
				e.logger.WarnContext(ctx, "report delivery failed", slog.String("network", string(t.Network())),
					slog.Int64("entry_id", entry.ID()), slog.Int("attempts", entry.Attempts()), slog.String("status", string(entry.Status())),
					slog.Any("error", cause))
			}

			if _, err := e.repo.Save(ctx, entry, owner); err != nil {
				return sent, err
			}
		}
	}

	return sent, ctx.Err()
}

// NetworkStatus is the state of one network (§6.10 GET /reporting/status).
type NetworkStatus struct {
	domain.NetworkStats
	Enabled bool
}

// Status returns the queue of every network.
func (e *Engine) Status(ctx context.Context) ([]NetworkStatus, error) {
	stats, err := e.repo.Stats(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]NetworkStatus, len(stats))
	for i, s := range stats {
		out[i] = NetworkStatus{NetworkStats: s, Enabled: e.Enabled(s.Network)}
	}

	return out, nil
}
