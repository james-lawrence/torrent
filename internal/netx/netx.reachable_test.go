package netx_test

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func TestReachableZeroAddrPort(t *testing.T) {
	zero := netip.AddrPort{}
	for _, tc := range []struct {
		dst  netip.AddrPort
		from netip.AddrPort
		want bool
	}{
		// zero is always unreachable
		{zero, zero, false},
		{zero, netip.MustParseAddrPort("8.8.8.8:0"), false},
		{netip.MustParseAddrPort("8.8.8.8:0"), zero, false},
		{zero, netip.MustParseAddrPort("[2001:db8::1]:0"), false},
		{zero, netip.MustParseAddrPort("[::1]:0"), false},
	} {
		got := netx.Reachable(tc.dst, tc.from)
		require.Equalf(t, tc.want, got, "Reachable(%s, %s)", tc.dst, tc.from)
	}
}

func TestReachable(t *testing.T) {
	for _, tc := range []struct {
		dst  string
		from string
		want bool
	}{
		// loopback ↔ loopback (true)
		{"127.0.0.1:0", "127.0.0.1:1", true},
		{"[::1]:0", "[::1]:1", true},
		// loopback ↔ private (false)
		{"127.0.0.1:0", "192.168.1.1:0", false},
		{"192.168.1.5:0", "127.0.0.1:0", false},
		// loopback ↔ public (false)
		{"127.0.0.1:0", "8.8.8.8:0", false},
		{"8.8.8.8:0", "127.0.0.1:0", false},
		// link-local ↔ link-local (true)
		{"169.254.1.1:0", "169.254.1.2:0", true},
		{"[fe80::1]:0", "[fe80::2]:0", true},
		// link-local ↔ private (false)
		{"169.254.1.1:0", "192.168.1.1:0", false},
		{"[fe80::1]:0", "[fc00::1]:0", false},
		// link-local ↔ loopback (false)
		{"169.254.1.1:0", "127.0.0.1:0", false},
		// routed ↔ routed: private ↔ private (true)
		{"192.168.1.5:0", "192.168.1.1:0", true},
		{"10.0.0.1:0", "172.16.0.1:0", true},
		{"[fc00::1]:0", "[fd00::1]:0", true},
		// routed ↔ routed: private ↔ public (true — NAT/routing)
		{"8.8.8.8:0", "192.168.1.1:0", true},
		{"192.168.1.1:0", "8.8.8.8:0", true},
		// routed ↔ routed: public ↔ public (true)
		{"8.8.8.8:0", "1.1.1.1:0", true},
		{"[2001:db8::1]:0", "[2001:db8::2]:0", true},
		// public ↔ loopback (false)
		{"[2001:db8::1]:0", "[::1]:0", false},
		// cross-family (false)
		{"127.0.0.1:0", "[::1]:0", false},
		{"8.8.8.8:0", "[2001:db8::1]:0", false},
		{"192.168.1.1:0", "[fc00::1]:0", false},
		// IPv4-in-IPv6 treated as IPv4
		{"[::ffff:127.0.0.1]:0", "127.0.0.1:0", true},
		{"[::ffff:8.8.8.8]:0", "192.168.1.1:0", true},
	} {
		dst := netip.MustParseAddrPort(tc.dst)
		from := netip.MustParseAddrPort(tc.from)
		got := netx.Reachable(dst, from)
		require.Equalf(t, tc.want, got, "Reachable(%s, %s)", tc.dst, tc.from)
	}
}
