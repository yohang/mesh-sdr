package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// EnrollmentTarget is a pending node the hub tries to enroll.
type EnrollmentTarget struct {
	ID  domain.NodeID
	URL domain.NodeURL
	Key domain.EnrollmentKey
}

// Enroller runs the §4.2 exchange with one node and returns the identity
// of the certificate it issued and delivered.
type Enroller interface {
	Enroll(ctx context.Context, t EnrollmentTarget) (domain.CertInfo, error)
}

// ErrEnrollmentRejected is returned by an Enroller when the node refused
// the exchange or answered with an invalid proof.
var ErrEnrollmentRejected = errors.New("enrollment_rejected")

// Enrollment drives the hub side of node enrollment (§4.2, ADR 0008).
type Enrollment struct {
	repo     domain.NodeRepository
	tx       Transactor
	enroller Enroller
	audit    Auditor
	now      Clock
	logger   *slog.Logger
	interval time.Duration

	// Enrolled is called after a node got enrolled (opens its control
	// channel); it may be nil.
	Enrolled func(ctx context.Context, id domain.NodeID)
}

// NewEnrollment returns the service. interval is the scan period of the
// worker.
func NewEnrollment(repo domain.NodeRepository, tx Transactor, enroller Enroller, audit Auditor, now Clock, interval time.Duration, logger *slog.Logger) *Enrollment {
	return &Enrollment{repo: repo, tx: tx, enroller: enroller, audit: audit, now: now, interval: interval, logger: logger}
}

// Targets returns the pending, enabled nodes whose token has not expired.
func (s *Enrollment) Targets(ctx context.Context) ([]EnrollmentTarget, error) {
	nodes, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()

	var out []EnrollmentTarget

	for _, n := range nodes {
		if n.Disabled() {
			continue
		}

		k, err := n.EnrollmentKey(now)
		if err != nil {
			continue
		}

		out = append(out, EnrollmentTarget{ID: n.ID(), URL: n.URL(), Key: k})
	}

	return out, nil
}

// Attempt runs one enrollment exchange with t and records the result.
func (s *Enrollment) Attempt(ctx context.Context, t EnrollmentTarget) error {
	cert, err := s.enroller.Enroll(ctx, t)
	if err != nil {
		if errors.Is(err, ErrEnrollmentRejected) {
			s.audit.Record(ctx, AuditRecord{ActorKind: ActorSystem, Action: "node.enroll", Target: t.ID.String(), Result: ResultDenied,
				Detail: map[string]string{"reason": err.Error()}})
		}

		return fmt.Errorf("enroll node %s: %w", t.ID, err)
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := s.repo.Get(ctx, t.ID)
		if err != nil {
			return err
		}

		v := n.Version()
		if err := n.CompleteEnrollment(t.Key, t.URL, cert, s.now()); err != nil {
			return err
		}

		return s.repo.Save(ctx, n, v)
	})
	if err != nil {
		s.audit.Record(ctx, AuditRecord{ActorKind: ActorSystem, Action: "node.enroll", Target: t.ID.String(), Result: ResultError,
			Detail: map[string]string{"reason": err.Error()}})

		return fmt.Errorf("record enrollment of node %s: %w", t.ID, err)
	}

	s.audit.Record(ctx, AuditRecord{ActorKind: ActorSystem, Action: "node.enroll", Target: t.ID.String(), Result: ResultOK,
		Detail: map[string]string{"cert_serial": cert.Serial()}})
	s.logger.InfoContext(ctx, "node enrolled", slog.String("node_id", t.ID.String()), slog.String("cert_serial", cert.Serial()))

	if s.Enrolled != nil {
		s.Enrolled(ctx, t.ID)
	}

	return nil
}

// Run scans pending nodes every interval and tries to enroll them, with a
// per-node jittered exponential back-off (1 s → 60 s). It returns when ctx
// is done.
func (s *Enrollment) Run(ctx context.Context) {
	type state struct {
		next  time.Time
		delay time.Duration
	}

	backoff := map[domain.NodeID]*state{}
	tick := time.NewTicker(s.interval)

	defer tick.Stop()

	for {
		targets, err := s.Targets(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "list enrollment targets", slog.Any("error", err))
		}

		now := time.Now()

		for _, t := range targets {
			st := backoff[t.ID]
			if st != nil && now.Before(st.next) {
				continue
			}

			actx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := s.Attempt(actx, t)

			cancel()

			if err == nil {
				delete(backoff, t.ID)

				continue
			}

			if st == nil {
				st = &state{delay: time.Second}
				backoff[t.ID] = st
			} else {
				st.delay = min(2*st.delay, time.Minute)
			}

			st.next = now.Add(Jitter(st.delay))

			level := slog.LevelDebug
			if errors.Is(err, ErrEnrollmentRejected) {
				level = slog.LevelWarn
			}

			s.logger.LogAttrs(ctx, level, "node enrollment attempt failed", slog.String("node_id", t.ID.String()),
				slog.Duration("retry_in", st.delay), slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Jitter returns a random duration in [d/2, d] (jittered back-off).
func Jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}

	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not security
}
