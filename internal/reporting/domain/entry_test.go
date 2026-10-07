package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/reporting/domain"
)

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 7: 32 * time.Minute, 8: time.Hour, 20: time.Hour,
	} {
		if got := domain.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestEntryLifecycle(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	if _, err := domain.NewEntry("nowhere", []byte(`{}`), domain.DedupKey{}, nil, t0); !errors.Is(err, domain.ErrInvalidEntry) {
		t.Errorf("unknown network: %v", err)
	}

	if _, err := domain.NewEntry(domain.NetworkMQTT, []byte(`{`), domain.DedupKey{}, nil, t0); !errors.Is(err, domain.ErrInvalidEntry) {
		t.Errorf("invalid payload: %v", err)
	}

	e, err := domain.NewEntry(domain.NetworkMQTT, []byte(`{"mode":"FT8"}`), domain.DedupKey{1}, nil, t0)
	if err != nil {
		t.Fatal(err)
	}

	if e.Status() != domain.StatusPending || !e.NextAttempt().Equal(t0) {
		t.Errorf("new entry = %+v", e.Snapshot())
	}

	p := domain.Policy{MaxAttempts: 3, MaxAge: 24 * time.Hour, Lease: time.Minute}

	e.Failed("refused", p, t0)
	if e.Status() != domain.StatusFailed || e.Attempts() != 1 || !e.NextAttempt().Equal(t0.Add(30*time.Second)) || e.LastError() != "refused" {
		t.Errorf("failed once = %+v", e.Snapshot())
	}

	e.Failed("refused", p, t0)
	e.Failed("refused", p, t0)

	if e.Status() != domain.StatusDead {
		t.Errorf("after max attempts = %s", e.Status())
	}

	old, _ := domain.NewEntry(domain.NetworkMQTT, []byte(`{}`), domain.DedupKey{2}, nil, t0)
	old.Failed("timeout", p, t0.Add(25*time.Hour))

	if old.Status() != domain.StatusDead {
		t.Errorf("after max age = %s", old.Status())
	}

	sent, _ := domain.NewEntry(domain.NetworkMQTT, []byte(`{}`), domain.DedupKey{3}, nil, t0)
	sent.Sent(t0.Add(time.Second))

	if sent.Status() != domain.StatusSent || !sent.SentAt().Equal(t0.Add(time.Second)) {
		t.Errorf("sent = %+v", sent.Snapshot())
	}
}
