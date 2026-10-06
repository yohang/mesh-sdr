// Package ratelimit holds the in-memory token buckets of the gateway authz
// (TECHNICAL_SPEC §5.12, §7.3 "What may stay in RAM"): one bucket per key,
// at most Capacity keys (least recently used evicted).
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Capacity bounds the number of keys of a limiter.
const Capacity = 100_000

// Limiter is a token bucket per key; it implements the grid app.RateLimiter.
type Limiter struct {
	mu    sync.Mutex
	limit rate.Limit
	burst int
	order *list.List // front = most recent
	items map[string]*list.Element
}

type item struct {
	key string
	lim *rate.Limiter
}

// New allows burst events per key, then one per every.
func New(every time.Duration, burst int) *Limiter {
	return &Limiter{limit: rate.Every(every), burst: max(burst, 1), order: list.New(), items: map[string]*list.Element{}}
}

// Allow takes one token of key at now. When none is left it returns false
// and the wait until the next one.
func (l *Limiter) Allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var lim *rate.Limiter

	if el, ok := l.items[key]; ok {
		l.order.MoveToFront(el)
		lim = el.Value.(*item).lim //nolint:forcetypeassert // only *item is stored
	} else {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.items[key] = l.order.PushFront(&item{key: key, lim: lim})

		if l.order.Len() > Capacity {
			last := l.order.Back()
			l.order.Remove(last)
			delete(l.items, last.Value.(*item).key) //nolint:forcetypeassert // only *item is stored
		}
	}

	r := lim.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)

		return false, d
	}

	return true, 0
}
