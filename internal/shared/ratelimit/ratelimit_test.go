package ratelimit_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestIPLimiter(t *testing.T) {
	l := ratelimit.NewIP(12*time.Second, 5, 10)
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

	if ok, _ := l.Allow(ip, t0.Add(12*time.Second)); !ok {
		t.Error("token not refilled")
	}

	if ok, _ := l.Allow(netip.MustParseAddr("::ffff:192.0.2.1"), t0.Add(12*time.Second)); ok {
		t.Error("mapped address has its own bucket")
	}

	if ok, _ := l.Allow(netip.MustParseAddr("192.0.2.2"), t0); !ok {
		t.Error("another client is limited")
	}

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
	l := ratelimit.NewIP(time.Minute, 1, 10)

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
	c := ratelimit.NewLRU[int, int](3)
	for i := range 5 {
		c.Put(i, i)
	}

	if c.Len() != 3 {
		t.Errorf("len = %d", c.Len())
	}

	if _, ok := c.Get(0); ok {
		t.Error("oldest key kept")
	}

	c.Get(2)
	c.Put(9, 9)

	if _, ok := c.Get(2); !ok {
		t.Error("recently used key evicted")
	}
}

func TestLimiterEvictsKeys(t *testing.T) {
	l := ratelimit.New[string](time.Hour, 1, 2)

	l.Allow("a", t0)
	l.Allow("b", t0)
	l.Allow("c", t0) // evicts a

	if ok, _ := l.Allow("a", t0); !ok {
		t.Error("evicted key kept its bucket")
	}

	if ok, _ := l.Allow("c", t0); ok {
		t.Error("recent key lost its bucket")
	}
}

func TestDynamicIPLimiter(t *testing.T) {
	every, burst := time.Minute, 1
	l := ratelimit.NewDynamicIP(func() (time.Duration, int) { return every, burst }, 10)
	ip := netip.MustParseAddr("192.0.2.7")

	if ok, _ := l.Allow(ip, t0); !ok {
		t.Fatal("first attempt refused")
	}

	if ok, _ := l.Allow(ip, t0); ok {
		t.Fatal("second attempt allowed with burst 1")
	}

	// The admin raises the limit: the next attempts refill at the new
	// rate (one per second) instead of one per minute.
	every, burst = time.Second, 5

	_, _ = l.Allow(ip, t0.Add(time.Second))

	if ok, _ := l.Allow(ip, t0.Add(3*time.Second)); !ok {
		t.Error("attempt refused after the rate was raised")
	}
}

func TestKeyLimiter(t *testing.T) {
	l := ratelimit.New[string](time.Hour, 2, 10)

	for i := range 2 {
		if ok, _ := l.Allow("alice", t0); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}

	if ok, wait := l.Allow("alice", t0); ok || wait <= 0 {
		t.Error("third attempt allowed")
	}

	if ok, _ := l.Allow("bob", t0); !ok {
		t.Error("another key refused")
	}

	if ok, _ := l.Allow("alice", t0.Add(time.Hour)); !ok {
		t.Error("refill missing")
	}
}
