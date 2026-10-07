package memory

import (
	"fmt"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestThrottle(t *testing.T) {
	th := NewThrottle(10)
	p := domain.DefaultThrottlePolicy()

	for i := 1; i <= 10; i++ {
		blocked, _, locked := th.Reserve("nobody", t0.Add(time.Duration(i)*time.Minute), p)
		if blocked || locked != (i == 10) {
			t.Errorf("attempt %d: blocked %v locked %v", i, blocked, locked)
		}
	}

	at := t0.Add(10 * time.Minute)

	blocked, until, _ := th.Reserve("nobody", at, p)
	if !blocked || until.Sub(at) != 15*time.Minute {
		t.Errorf("after 10 failures: blocked %v for %v", blocked, until.Sub(at))
	}

	if blocked, _, _ := th.Reserve("nobody", at.Add(16*time.Minute), p); blocked {
		t.Error("still blocked after the lock")
	}

	th.Reset("nobody")

	if blocked, _, _ := th.Reserve("nobody", at, p); blocked {
		t.Error("reset kept the lock")
	}

	for i := range 20 {
		th.Reserve(fmt.Sprint(i), t0, p)
	}

	if th.cache.Len() != 10 {
		t.Errorf("throttle not bounded: %d", th.cache.Len())
	}
}

func TestRefusalGate(t *testing.T) {
	g := NewRefusalGate(10)

	if !g.First("ip:x", t0.Add(time.Minute), t0) {
		t.Error("first refusal not reported")
	}

	if g.First("ip:x", t0.Add(2*time.Minute), t0.Add(30*time.Second)) {
		t.Error("second refusal of the window reported")
	}

	if !g.First("ip:y", t0.Add(time.Minute), t0) {
		t.Error("another key shares the window")
	}

	if !g.First("ip:x", t0.Add(3*time.Minute), t0.Add(time.Minute)) {
		t.Error("refusal of the next window not reported")
	}
}
