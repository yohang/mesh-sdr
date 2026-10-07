// Package ratelimit holds the in-memory rate limiters of the hub (TECHNICAL_SPEC
// §5.12, §7.3 "What may stay in RAM"): token buckets per key, bounded by an
// LRU (least recently used key evicted). Losing them on restart only resets
// throttling.
package ratelimit

import (
	"container/list"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// DefaultCapacity bounds the keys of a limiter (spec: LRU, 100 000 entries).
const DefaultCapacity = 100_000

// LRU is a size-bounded map evicting the least recently used key. It is not
// safe for concurrent use.
type LRU[K comparable, V any] struct {
	capacity int
	order    *list.List // front = most recent
	items    map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key K
	val V
}

// NewLRU returns an LRU of at most capacity keys (at least one).
func NewLRU[K comparable, V any](capacity int) *LRU[K, V] {
	return &LRU[K, V]{capacity: max(capacity, 1), order: list.New(), items: map[K]*list.Element{}}
}

// Get returns the value of k and marks it recently used.
func (c *LRU[K, V]) Get(k K) (V, bool) {
	el, ok := c.items[k]
	if !ok {
		var zero V

		return zero, false
	}

	c.order.MoveToFront(el)

	return el.Value.(*entry[K, V]).val, true //nolint:forcetypeassert // only *entry is stored
}

// Put sets the value of k, evicting the least recently used key when full.
func (c *LRU[K, V]) Put(k K, v V) {
	if el, ok := c.items[k]; ok {
		el.Value.(*entry[K, V]).val = v //nolint:forcetypeassert // only *entry is stored
		c.order.MoveToFront(el)

		return
	}

	c.items[k] = c.order.PushFront(&entry[K, V]{key: k, val: v})

	if c.order.Len() > c.capacity {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry[K, V]).key) //nolint:forcetypeassert // only *entry is stored
	}
}

// Remove deletes k.
func (c *LRU[K, V]) Remove(k K) {
	if el, ok := c.items[k]; ok {
		c.order.Remove(el)
		delete(c.items, k)
	}
}

// Len returns the number of keys.
func (c *LRU[K, V]) Len() int { return c.order.Len() }

// RateFunc returns the current rate: one event per every, after a burst.
type RateFunc func() (every time.Duration, burst int)

// Limiter is a token bucket per key, at most capacity keys. Safe for
// concurrent use.
type Limiter[K comparable] struct {
	mu    sync.Mutex
	limit rate.Limit
	burst int
	rate  RateFunc
	cache *LRU[K, *rate.Limiter]
}

// New allows burst events per key, then one per every.
func New[K comparable](every time.Duration, burst, capacity int) *Limiter[K] {
	return &Limiter[K]{limit: rate.Every(every), burst: max(burst, 1), cache: NewLRU[K, *rate.Limiter](capacity)}
}

// NewDynamic reads its rate from r at every call (a settings value): a
// change applies to the next event of every key, keeping the tokens each
// key has left. A non-positive rate keeps the previous one.
func NewDynamic[K comparable](r RateFunc, capacity int) *Limiter[K] {
	every, burst := r()
	l := New[K](every, burst, capacity)
	l.rate = r

	return l
}

// Allow takes one token of key at now. When none is left it returns false
// and the wait until the next one.
func (l *Limiter[K]) Allow(key K, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rate != nil {
		if every, burst := l.rate(); every > 0 && burst > 0 {
			l.limit, l.burst = rate.Every(every), burst
		}
	}

	lim, ok := l.cache.Get(key)
	if !ok {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.cache.Put(key, lim)
	}

	if lim.Limit() != l.limit {
		lim.SetLimitAt(now, l.limit)
	}

	if lim.Burst() != l.burst {
		lim.SetBurstAt(now, l.burst)
	}

	r := lim.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)

		return false, d
	}

	return true, 0
}

// IPLimiter is a Limiter keyed by client: the address for IPv4, its /64
// network for IPv6 (one client usually holds a whole /64). Requests whose
// address is unknown share one bucket.
type IPLimiter struct{ l *Limiter[netip.Prefix] }

// NewIP allows burst events per client, then one per every.
func NewIP(every time.Duration, burst, capacity int) *IPLimiter {
	return &IPLimiter{l: New[netip.Prefix](every, burst, capacity)}
}

// NewDynamicIP is NewIP with the rate read from r (see NewDynamic).
func NewDynamicIP(r RateFunc, capacity int) *IPLimiter {
	return &IPLimiter{l: NewDynamic[netip.Prefix](r, capacity)}
}

// Allow takes one token of the client at ip.
func (l *IPLimiter) Allow(ip netip.Addr, now time.Time) (bool, time.Duration) {
	return l.l.Allow(IPKey(ip), now)
}

// IPKey is the key of a client address: the address for IPv4, its /64
// network for IPv6; the zero prefix when the address is unknown.
func IPKey(ip netip.Addr) netip.Prefix {
	if !ip.IsValid() {
		return netip.Prefix{}
	}

	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}

	p, _ := ip.Prefix(64)

	return p
}
