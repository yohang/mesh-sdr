package process

import (
	"math/rand/v2"
	"time"
)

// RestartPolicy describes back-off and crash-loop rules. Two presets mirror
// the spec: DevicePolicy (§8.2) and DecoderPolicy (§8.4).
type RestartPolicy struct {
	// Steps, when set, is an explicit delay sequence; the last value repeats.
	Steps []time.Duration
	// Otherwise exponential: Initial * Factor^(attempt-1), capped at Max.
	Initial time.Duration
	Factor  float64
	Max     time.Duration
	// Jitter is a ± fraction applied to every delay (0.2 = ±20 %). The spec
	// has no jitter; it avoids synchronised restarts of many sessions after a
	// shared cause (USB reset, codecserver restart).
	Jitter float64
	// MaxAttempts > 0 stops restarting after that many consecutive failed
	// attempts (device: 10, then Failed). 0 = unlimited.
	MaxAttempts int
	// ResetAfter: a run that was ready and lasted at least this long resets
	// the attempt counter. 0 = only readiness resets it. Spec is silent.
	ResetAfter time.Duration
	// Crash loop: CrashLoopCount unexpected exits within CrashLoopWindow stop
	// normal restarts; one retry every CrashLoopRetry, or on Kick (§8.4).
	CrashLoopCount  int
	CrashLoopWindow time.Duration
	CrashLoopRetry  time.Duration

	// rnd returns a value in [0,1); tests may override it.
	rnd func() float64
}

// DevicePolicy is §8.2: 2 s, 5 s, 15 s, 30 s, then 60 s; 10 attempts; then
// Failed (auto-recover every 15 min is the device manager's job).
// Note: FEATURE_SPEC SRC-004 says "every 15 s, up to 10 attempts".
func DevicePolicy() RestartPolicy {
	return RestartPolicy{
		Steps:       []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second},
		Jitter:      0.1,
		MaxAttempts: 10,
		ResetAfter:  60 * time.Second,
	}
}

// DecoderPolicy is §8.4: 1 s, 2 s, 4 s … capped at 60 s; ≥ 5 unexpected
// exits within 5 min = crash loop, then one retry every 10 min.
func DecoderPolicy() RestartPolicy {
	return RestartPolicy{
		Initial:         time.Second,
		Factor:          2,
		Max:             60 * time.Second,
		Jitter:          0.1,
		ResetAfter:      60 * time.Second,
		CrashLoopCount:  5,
		CrashLoopWindow: 5 * time.Minute,
		CrashLoopRetry:  10 * time.Minute,
	}
}

// Delay returns the back-off before attempt n (1-based).
func (p RestartPolicy) Delay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	var d time.Duration
	if len(p.Steps) > 0 {
		i := min(n-1, len(p.Steps)-1)
		d = p.Steps[i]
	} else {
		f := p.Factor
		if f < 1 {
			f = 1
		}
		fd := float64(p.Initial)
		for i := 1; i < n && (p.Max == 0 || fd < float64(p.Max)); i++ {
			fd *= f
		}
		d = time.Duration(fd)
		if p.Max > 0 && d > p.Max {
			d = p.Max
		}
	}
	return p.jitter(d)
}

func (p RestartPolicy) jitter(d time.Duration) time.Duration {
	if p.Jitter <= 0 || d <= 0 {
		return d
	}
	r := rand.Float64
	if p.rnd != nil {
		r = p.rnd
	}
	// uniform in [d*(1-j), d*(1+j))
	return time.Duration(float64(d) * (1 - p.Jitter + 2*p.Jitter*r()))
}

// crashWindow tracks unexpected exits in a sliding window.
type crashWindow struct {
	times []time.Time
}

func (w *crashWindow) add(now time.Time, p RestartPolicy) bool {
	if p.CrashLoopCount <= 0 {
		return false
	}
	w.times = append(w.times, now)
	cut := now.Add(-p.CrashLoopWindow)
	i := 0
	for i < len(w.times) && w.times[i].Before(cut) {
		i++
	}
	w.times = w.times[i:]
	return len(w.times) >= p.CrashLoopCount
}

func (w *crashWindow) reset() { w.times = w.times[:0] }
