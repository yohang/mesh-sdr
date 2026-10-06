// Package memory holds the in-memory login rate limiters (TECHNICAL_SPEC
// §7.3 "What may stay in RAM": per-IP token buckets, LRU-bounded). Losing
// them on restart only resets throttling; account lock-outs of existing
// users are persisted in the users table.
package memory

import (
	"container/list"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// DefaultCapacity bounds each limiter (spec: LRU, 100 000 entries).
const DefaultCapacity = 100_000

// lru is a size-bounded map evicting the least recently used key.
type lru[K comparable, V any] struct {
	capacity int
	order    *list.List // front = most recent
	items    map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lru[K, V] {
	return &lru[K, V]{capacity: max(capacity, 1), order: list.New(), items: map[K]*list.Element{}}
}

func (c *lru[K, V]) get(k K) (V, bool) {
	el, ok := c.items[k]
	if !ok {
		var zero V

		return zero, false
	}

	c.order.MoveToFront(el)

	return el.Value.(*entry[K, V]).val, true
}

func (c *lru[K, V]) put(k K, v V) {
	if el, ok := c.items[k]; ok {
		el.Value.(*entry[K, V]).val = v
		c.order.MoveToFront(el)

		return
	}

	c.items[k] = c.order.PushFront(&entry[K, V]{key: k, val: v})

	if c.order.Len() > c.capacity {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry[K, V]).key)
	}
}

func (c *lru[K, V]) remove(k K) {
	if el, ok := c.items[k]; ok {
		c.order.Remove(el)
		delete(c.items, k)
	}
}

func (c *lru[K, V]) len() int { return c.order.Len() }

// IPLimiter is a token bucket per client address. IPv6 clients are keyed by
// their /64, which one subscriber usually controls entirely.
type IPLimiter struct {
	mu    sync.Mutex
	limit rate.Limit
	burst int
	cache *lru[netip.Prefix, *rate.Limiter]
}

// NewIPLimiter allows burst attempts, then one per every, per client.
func NewIPLimiter(every time.Duration, burst, capacity int) *IPLimiter {
	return &IPLimiter{limit: rate.Every(every), burst: max(burst, 1), cache: newLRU[netip.Prefix, *rate.Limiter](capacity)}
}

func ipKey(ip netip.Addr) netip.Prefix {
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}

	p, _ := ip.Prefix(64)

	return p
}

// Allow takes one token for ip. When none is left it returns false and the
// wait until the next one.
func (l *IPLimiter) Allow(ip netip.Addr, now time.Time) (bool, time.Duration) {
	if !ip.IsValid() {
		return true, 0
	}

	key := ipKey(ip)

	l.mu.Lock()
	defer l.mu.Unlock()

	lim, ok := l.cache.get(key)
	if !ok {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.cache.put(key, lim)
	}

	r := lim.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)

		return false, d
	}

	return true, 0
}

// Throttle applies the account throttle policy to login identifiers that
// match no account, so that responses for unknown and existing accounts are
// identical (SR-05).
type Throttle struct {
	mu    sync.Mutex
	cache *lru[string, throttleState]
}

type throttleState struct {
	failures int
	until    time.Time
}

// NewThrottle returns a throttle holding at most capacity identifiers.
func NewThrottle(capacity int) *Throttle {
	return &Throttle{cache: newLRU[string, throttleState](capacity)}
}

// Reserve refuses an attempt for key while it is delayed or locked, or
// counts it as a failure; Reset clears the count after a success.
func (t *Throttle) Reserve(key string, now time.Time, p domain.ThrottlePolicy) (bool, time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, _ := t.cache.get(key)
	if s.until.After(now) {
		return true, s.until, false
	}

	s.failures++
	s.until = p.BlockedUntil(s.failures, now)
	t.cache.put(key, s)

	return false, time.Time{}, p.Locks(s.failures)
}

// Reset forgets key.
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cache.remove(key)
}

// RefusalGate remembers, per key, the end of the window whose first refusal
// was audited (app.RefusalGate).
type RefusalGate struct {
	mu    sync.Mutex
	cache *lru[string, time.Time]
}

// NewRefusalGate returns a gate holding at most capacity keys.
func NewRefusalGate(capacity int) *RefusalGate {
	return &RefusalGate{cache: newLRU[string, time.Time](capacity)}
}

// First reports whether this refusal opens a new window for key.
func (g *RefusalGate) First(key string, until, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if end, ok := g.cache.get(key); ok && end.After(now) {
		return false
	}

	g.cache.put(key, until)

	return true
}
