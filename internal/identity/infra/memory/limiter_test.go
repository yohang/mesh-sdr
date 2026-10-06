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

	if ok, _ := l.Allow(netip.Addr{}, t0); !ok {
		t.Error("unknown address limited")
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
		locked := th.RecordFailure("nobody", t0, p)
		if locked != (i == 10) {
			t.Errorf("failure %d locked = %v", i, locked)
		}
	}

	if until := th.BlockedUntil("nobody", t0); until.Sub(t0) != 15*time.Minute {
		t.Errorf("blocked for %v", until.Sub(t0))
	}

	if !th.BlockedUntil("nobody", t0.Add(16*time.Minute)).IsZero() {
		t.Error("still blocked after the lock")
	}

	th.Reset("nobody")

	if !th.BlockedUntil("nobody", t0).IsZero() {
		t.Error("reset kept the lock")
	}

	for i := range 20 {
		th.RecordFailure(fmt.Sprint(i), t0, p)
	}

	if th.cache.len() != 10 {
		t.Errorf("throttle not bounded: %d", th.cache.len())
	}
}
