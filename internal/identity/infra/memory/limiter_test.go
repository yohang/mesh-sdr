package memory

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestIPLimiter(t *testing.T) {
	l := NewIPLimiter(12*time.Second, 5, 10)
	ip := netip.MustParseAddr("192.0.2.1")

	for i := range 5 {
		if ok, _ := l.Allow(ip, t0); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}

	ok, wait := l.Allow(ip, t0)
	if ok || wait != 12*time.Second {
		t.Fatalf("6th attempt = %v, wait %v", ok, wait)
	}

	// A refused attempt does not consume a token.
	if ok, _ := l.Allow(ip, t0.Add(12*time.Second)); !ok {
		t.Error("token not refilled")
	}

	// IPv4-mapped IPv6 is the same client.
	if ok, _ := l.Allow(netip.MustParseAddr("::ffff:192.0.2.1"), t0.Add(12*time.Second)); ok {
		t.Error("mapped address has its own bucket")
	}

	if ok, _ := l.Allow(netip.MustParseAddr("192.0.2.2"), t0); !ok {
		t.Error("another client is limited")
	}

	// Unknown addresses share one bucket.
	for i := range 5 {
		if ok, _ := l.Allow(netip.Addr{}, t0); !ok {
			t.Fatalf("unknown address: attempt %d refused", i+1)
		}
	}

	if ok, _ := l.Allow(netip.Addr{}, t0); ok {
		t.Error("unknown addresses not limited")
	}
}

func TestIPLimiterKeysIPv6By64(t *testing.T) {
	l := NewIPLimiter(time.Minute, 1, 10)

	if ok, _ := l.Allow(netip.MustParseAddr("2001:db8:1:2::1"), t0); !ok {
		t.Fatal("first attempt refused")
	}

	if ok, _ := l.Allow(netip.MustParseAddr("2001:db8:1:2:ffff::9"), t0); ok {
		t.Error("same /64 not limited")
	}

	if ok, _ := l.Allow(netip.MustParseAddr("2001:db8:1:3::1"), t0); !ok {
		t.Error("other /64 limited")
	}
}

func TestLRUEvicts(t *testing.T) {
	c := newLRU[int, int](3)
	for i := range 5 {
		c.put(i, i)
	}

	if c.len() != 3 {
		t.Errorf("len = %d", c.len())
	}

	if _, ok := c.get(0); ok {
		t.Error("oldest key kept")
	}

	c.get(2)
	c.put(9, 9)

	if _, ok := c.get(2); !ok {
		t.Error("recently used key evicted")
	}
}

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

	if th.cache.len() != 10 {
		t.Errorf("throttle not bounded: %d", th.cache.len())
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

func TestDynamicIPLimiter(t *testing.T) {
	every, burst := time.Minute, 1
	l := NewDynamicIPLimiter(func() (time.Duration, int) { return every, burst }, 10)
	ip := netip.MustParseAddr("192.0.2.7")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if ok, _ := l.Allow(ip, now); !ok {
		t.Fatal("first attempt refused")
	}

	if ok, _ := l.Allow(ip, now); ok {
		t.Fatal("second attempt allowed with burst 1")
	}

	// The admin raises the limit: the next attempts refill at the new
	// rate (one per second) instead of one per minute.
	every, burst = time.Second, 5

	_, _ = l.Allow(ip, now.Add(time.Second))

	if ok, _ := l.Allow(ip, now.Add(3*time.Second)); !ok {
		t.Error("attempt refused after the rate was raised")
	}
}
