package media

import (
	"time"
)

// Inbound message limits of the media WS (§6.9): 20 msg/s sustained, burst
// 50. Each refused message is a strike; repeated strikes close the
// connection with 4429.
const (
	msgBurst     = 50
	strikeLimit  = 10
	strikeWindow = time.Minute
)

// msgLimiter is the per-connection token bucket of inbound messages. It is
// used by the connection's read loop only.
type msgLimiter struct {
	rate, burst float64
	tokens      float64
	last        time.Time
	strikes     []time.Time
}

func newMsgLimiter(rate int) *msgLimiter {
	return &msgLimiter{rate: float64(rate), burst: msgBurst, tokens: msgBurst}
}

// allow takes a token at now. When none is left it returns the wait before
// the next one and whether the strikes reached the limit.
func (l *msgLimiter) allow(now time.Time) (ok bool, retry time.Duration, escalate bool) {
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}

	l.last = now

	if l.tokens >= 1 {
		l.tokens--

		return true, 0, false
	}

	cut := now.Add(-strikeWindow)
	kept := l.strikes[:0]

	for _, s := range l.strikes {
		if s.After(cut) {
			kept = append(kept, s)
		}
	}

	l.strikes = append(kept, now)
	retry = time.Duration((1 - l.tokens) / l.rate * float64(time.Second))

	return false, max(retry, time.Millisecond), len(l.strikes) >= strikeLimit
}
