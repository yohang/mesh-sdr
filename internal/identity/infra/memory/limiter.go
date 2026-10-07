// Package memory holds the in-memory login throttles of the identity module
// (TECHNICAL_SPEC §7.3 "What may stay in RAM", LRU-bounded): the failure
// throttle of unknown logins and the refusal gate. Losing them on restart
// only resets throttling; account lock-outs of existing users are persisted
// in the users table. The token buckets are internal/shared/ratelimit.
package memory

import (
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"
)

// Throttle applies the account throttle policy to login identifiers that
// match no account, so that responses for unknown and existing accounts are
// identical (SR-05).
type Throttle struct {
	mu    sync.Mutex
	cache *ratelimit.LRU[string, throttleState]
}

type throttleState struct {
	failures int
	until    time.Time
}

// NewThrottle returns a throttle holding at most capacity identifiers.
func NewThrottle(capacity int) *Throttle {
	return &Throttle{cache: ratelimit.NewLRU[string, throttleState](capacity)}
}

// Reserve refuses an attempt for key while it is delayed or locked, or
// counts it as a failure; Reset clears the count after a success.
func (t *Throttle) Reserve(key string, now time.Time, p domain.ThrottlePolicy) (bool, time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, _ := t.cache.Get(key)
	if s.until.After(now) {
		return true, s.until, false
	}

	s.failures++
	s.until = p.BlockedUntil(s.failures, now)
	t.cache.Put(key, s)

	return false, time.Time{}, p.Locks(s.failures)
}

// Reset forgets key.
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cache.Remove(key)
}

// RefusalGate remembers, per key, the end of the window whose first refusal
// was audited (app.RefusalGate).
type RefusalGate struct {
	mu    sync.Mutex
	cache *ratelimit.LRU[string, time.Time]
}

// NewRefusalGate returns a gate holding at most capacity keys.
func NewRefusalGate(capacity int) *RefusalGate {
	return &RefusalGate{cache: ratelimit.NewLRU[string, time.Time](capacity)}
}

// First reports whether this refusal opens a new window for key.
func (g *RefusalGate) First(key string, until, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if end, ok := g.cache.Get(key); ok && end.After(now) {
		return false
	}

	g.cache.Put(key, until)

	return true
}
