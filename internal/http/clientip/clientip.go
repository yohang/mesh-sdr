// Package clientip resolves the client address of a request (SR-13, SR-14,
// AUTH-016): the TCP peer, or, when the peer is a trusted proxy listed in
// http.trusted_proxies, the right-most X-Forwarded-For hop that is not a
// trusted proxy. Addresses are canonicalised (IPv4-mapped IPv6 to IPv4, no
// zone) and only compared against explicit CIDR lists.
package clientip

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type ctxKey struct{}

// Canonical returns a with IPv4-mapped IPv6 unmapped and the zone removed.
func Canonical(a netip.Addr) netip.Addr {
	if !a.IsValid() {
		return a
	}

	return a.Unmap().WithZone("")
}

// In reports whether a is in one of the prefixes.
func In(a netip.Addr, prefixes []netip.Prefix) bool {
	a = Canonical(a)
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}

	return false
}

// Resolver resolves client addresses behind trusted proxies.
type Resolver struct {
	trusted []netip.Prefix
}

// NewResolver returns a resolver trusting the given proxy networks.
func NewResolver(trusted []netip.Prefix) *Resolver {
	return &Resolver{trusted: trusted}
}

// Resolve returns the client address of r (invalid when unknown).
func (res *Resolver) Resolve(r *http.Request) netip.Addr {
	peer := peerAddr(r.RemoteAddr)
	if !peer.IsValid() || !In(peer, res.trusted) {
		return peer
	}

	// Right to left: the last hop appended by each trusted proxy.
	var last netip.Addr // right-most hop that parsed

	hops := forwardedFor(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			// A malformed hop: keep the right-most hop that parsed, never
			// the proxy itself (unknown when none parsed).
			return last
		}

		last = a
		if !In(a, res.trusted) {
			return a
		}
	}

	if last.IsValid() {
		return last
	}

	return peer // no forwarding header: the proxy is the client
}

// parseHop parses an X-Forwarded-For hop: an address, "ip:port" or
// "[v6]:port".
func parseHop(h string) (netip.Addr, bool) {
	h = strings.TrimSpace(h)

	if a, err := netip.ParseAddr(h); err == nil {
		return Canonical(a), true
	}

	if ap, err := netip.ParseAddrPort(h); err == nil {
		return Canonical(ap.Addr()), true
	}

	return netip.Addr{}, false
}

func forwardedFor(values []string) []string {
	var hops []string

	for _, v := range values {
		for h := range strings.SplitSeq(v, ",") {
			hops = append(hops, h)
		}
	}

	return hops
}

func peerAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}

	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}

	return Canonical(a)
}

// Middleware stores the resolved client address in the request context.
func (res *Resolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, res.Resolve(r))))
	})
}

// From returns the client address stored by Middleware (invalid when none).
func From(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(ctxKey{}).(netip.Addr)

	return a
}

// With returns ctx carrying a client address (tests, non-HTTP callers).
func With(ctx context.Context, a netip.Addr) context.Context {
	return context.WithValue(ctx, ctxKey{}, Canonical(a))
}
