package http_test

import (
	"testing"

	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
)

// TestMaskIP: IPv4 is masked to its /24 network, IPv6 to its /48 prefix
// (privacy.mask_ips); nothing else leaks.
func TestMaskIP(t *testing.T) {
	for _, tt := range []struct{ ip, want string }{
		{"192.0.2.77", "192.0.2.x"},
		{"203.0.113.255", "203.0.113.x"},
		{"0.0.0.1", "0.0.0.x"},
		{"2001:db8:1234:5678:9abc:def0:1234:5678", "2001:db8:1234::/48"},
		{"2001:db8::1", "2001:db8::/48"},
		{"fe80::1%eth0", "fe80::/48"},
		{"::ffff:192.0.2.77", "192.0.2.x"},
		{"", "—"},
		{"not an address", "—"},
		{"192.0.2.77:443", "—"},
	} {
		if got := gridhttp.MaskIP(tt.ip); got != tt.want {
			t.Errorf("MaskIP(%q) = %q, want %q", tt.ip, got, tt.want)
		}
	}
}
