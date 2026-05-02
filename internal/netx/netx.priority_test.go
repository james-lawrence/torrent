package netx_test

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/internal/netx"
)

func TestAddrPortPriority(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		priority int
	}{
		{"public IPv6", "[2001:db8::1]:0", 0},
		{"ULA IPv6", "[fc00::1]:0", 1},
		{"link-local IPv6", "[fe80::1]:0", 1},
		{"loopback IPv6", "[::1]:0", 1},
		{"public IPv4", "203.0.113.1:0", 2},
		{"RFC1918 10/8", "10.0.0.1:0", 3},
		{"RFC1918 172.16/12", "172.16.0.1:0", 3},
		{"RFC1918 192.168/16", "192.168.1.1:0", 3},
		{"link-local IPv4", "169.254.1.1:0", 3},
		{"loopback IPv4", "127.0.0.1:0", 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := netip.MustParseAddrPort(tc.addr)
			got := netx.AddrPortPriority(ap)
			if got != tc.priority {
				t.Errorf("AddrPortPriority(%s) = %d, want %d", tc.addr, got, tc.priority)
			}
		})
	}

	// verify ordering: public IPv6 < private IPv6 < public IPv4 < private IPv4
	ordered := []string{
		"[2001:db8::1]:0",
		"[fc00::1]:0",
		"203.0.113.1:0",
		"10.0.0.1:0",
	}
	for i := 1; i < len(ordered); i++ {
		prev := netx.AddrPortPriority(netip.MustParseAddrPort(ordered[i-1]))
		curr := netx.AddrPortPriority(netip.MustParseAddrPort(ordered[i]))
		if prev >= curr {
			t.Errorf("expected %s (pri %d) < %s (pri %d)", ordered[i-1], prev, ordered[i], curr)
		}
	}
}
