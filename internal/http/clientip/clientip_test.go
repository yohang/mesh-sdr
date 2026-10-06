package clientip_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/yohang/mesh-sdr/internal/http/clientip"
)

func TestResolve(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}

	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"no proxy", "203.0.113.7:4242", nil, "203.0.113.7"},
		{"forged header from an untrusted peer", "203.0.113.7:4242", []string{"198.51.100.1"}, "203.0.113.7"},
		{"one proxy", "10.0.0.2:80", []string{"198.51.100.1"}, "198.51.100.1"},
		{"two proxies", "10.0.0.2:80", []string{"198.51.100.1, 10.0.0.3"}, "198.51.100.1"},
		{"two proxies, two headers", "10.0.0.2:80", []string{"198.51.100.1", "10.0.0.3"}, "198.51.100.1"},
		{"spoofed left-most hop", "10.0.0.2:80", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"spoofed trusted address on the left", "10.0.0.2:80", []string{"10.9.9.9, 198.51.100.1, 10.0.0.3"}, "198.51.100.1"},
		{"only trusted hops", "10.0.0.2:80", []string{"10.0.0.4"}, "10.0.0.4"},
		{"no header from a proxy", "10.0.0.2:80", nil, "10.0.0.2"},
		{"malformed hop", "10.0.0.2:80", []string{"198.51.100.1, garbage"}, "10.0.0.2"},
		{"ipv4-mapped peer", "[::ffff:203.0.113.7]:4242", nil, "203.0.113.7"},
		{"ipv4-mapped hop", "[fd00::1]:80", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"zone removed", "[fe80::1%eth0]:80", nil, "fe80::1"},
		{"ipv6 client", "[fd00::1]:80", []string{"2001:db8::7"}, "2001:db8::7"},
	}

	res := clientip.NewResolver(trusted)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote

			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}

			r.Header.Set("Forwarded", "for=192.0.2.66") // never honoured

			if got := res.Resolve(r); got.String() != tt.want {
				t.Errorf("Resolve = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	var got netip.Addr

	h := clientip.NewResolver(nil).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = clientip.From(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.5:1"
	h.ServeHTTP(httptest.NewRecorder(), r)

	if got.String() != "192.0.2.5" {
		t.Errorf("From = %s", got)
	}
}

func TestIn(t *testing.T) {
	any4 := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")}

	if !clientip.In(netip.MustParseAddr("::ffff:192.168.1.1"), any4) {
		t.Error("mapped address not matched")
	}

	if clientip.In(netip.MustParseAddr("10.0.0.1"), any4) || clientip.In(netip.Addr{}, any4) {
		t.Error("address outside the list matched")
	}
}
