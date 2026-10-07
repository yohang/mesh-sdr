package app

import (
	"net/netip"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/events/domain"
	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"
)

// Limits are the caps of the hub events WebSocket (ADR 0016 decision 14,
// ADR 0018): concurrent sockets per session, per client address and for
// the whole hub, and upgrades per client address and minute (§5.12).
type Limits struct {
	PerSession        int
	PerAddress        int
	Total             int
	UpgradesPerMinute int
}

// DefaultLimits are the M0 values. The hub must sustain 2 000 concurrent
// events sockets (§2.3); a session rarely has more than a few tabs.
func DefaultLimits() Limits {
	return Limits{PerSession: 16, PerAddress: 64, Total: 4000, UpgradesPerMinute: 30}
}

// Admission admits event sockets within the Limits. Safe for concurrent
// use.
type Admission struct {
	limits Limits

	mu        sync.Mutex
	total     int
	sessions  map[string]int
	addresses map[string]int

	upgrades *ratelimit.Limiter[string] // nil: upgrades not limited
}

// NewAdmission returns the admission of limits.
func NewAdmission(limits Limits) *Admission {
	a := &Admission{limits: limits, sessions: map[string]int{}, addresses: map[string]int{}}

	if n := limits.UpgradesPerMinute; n > 0 {
		// Client addresses whose upgrade rate is tracked: the least
		// recently seen are evicted.
		a.upgrades = ratelimit.New[string](time.Minute/time.Duration(n), n, ratelimit.DefaultCapacity)
	}

	return a
}

// Admit admits a socket of session (its public handle, "" when anonymous)
// from address at now (IPv6 addresses count per /64, AddressKey). It returns the release function to call when the
// socket ends, or domain.ErrUpgradeRate with the wait before the next
// allowed upgrade, domain.ErrTooManyConnections or domain.ErrHubFull.
func (a *Admission) Admit(session, address string, now time.Time) (release func(), retryAfter time.Duration, err error) {
	address = AddressKey(address)

	a.mu.Lock()
	defer a.mu.Unlock()

	if wait := a.takeUpgrade(address, now); wait > 0 {
		return nil, wait, domain.ErrUpgradeRate
	}

	switch {
	case a.total >= a.limits.Total:
		return nil, 0, domain.ErrHubFull
	case session != "" && a.sessions[session] >= a.limits.PerSession:
		return nil, 0, domain.ErrTooManyConnections.WithDetail("too many open event connections for this session")
	case a.addresses[address] >= a.limits.PerAddress:
		return nil, 0, domain.ErrTooManyConnections.WithDetail("too many open event connections from this address")
	}

	a.total++
	a.addresses[address]++

	if session != "" {
		a.sessions[session]++
	}

	var once sync.Once

	return func() { once.Do(func() { a.release(session, address) }) }, 0, nil
}

func (a *Admission) release(session, address string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.total--

	if a.addresses[address]--; a.addresses[address] <= 0 {
		delete(a.addresses, address)
	}

	if session != "" {
		if a.sessions[session]--; a.sessions[session] <= 0 {
			delete(a.sessions, session)
		}
	}
}

// AddressKey is the key of a client address for the caps: the address for
// IPv4, its /64 network for IPv6, since one client usually holds a whole
// /64.
func AddressKey(address string) string {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return address
	}

	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}

	p, err := ip.Prefix(64)
	if err != nil {
		return address
	}

	return p.String()
}

// Open returns the number of admitted sockets.
func (a *Admission) Open() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.total
}

// takeUpgrade takes one upgrade token of address; it returns the wait
// before the next one when none is left.
func (a *Admission) takeUpgrade(address string, now time.Time) time.Duration {
	if a.upgrades == nil {
		return 0
	}

	_, wait := a.upgrades.Allow(address, now)

	return wait
}
