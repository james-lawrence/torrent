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
		{dst: zero, from: zero, want: false},
		{dst: zero, from: netip.MustParseAddrPort("8.8.8.8:0"), want: false},
		{dst: netip.MustParseAddrPort("8.8.8.8:0"), from: zero, want: false},
		{dst: zero, from: netip.MustParseAddrPort("[2001:db8::1]:0"), want: false},
		{dst: zero, from: netip.MustParseAddrPort("[::1]:0"), want: false},
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
		{dst: "127.0.0.1:0", from: "127.0.0.1:1", want: true},
		{dst: "[::1]:0", from: "[::1]:1", want: true},
		// loopback ↔ private (false)
		{dst: "127.0.0.1:0", from: "192.168.1.1:0", want: false},
		{dst: "192.168.1.5:0", from: "127.0.0.1:0", want: false},
		// loopback ↔ public (false)
		{dst: "127.0.0.1:0", from: "8.8.8.8:0", want: false},
		{dst: "8.8.8.8:0", from: "127.0.0.1:0", want: false},
		// link-local ↔ link-local (true)
		{dst: "169.254.1.1:0", from: "169.254.1.2:0", want: true},
		{dst: "[fe80::1]:0", from: "[fe80::2]:0", want: true},
		// link-local ↔ private (false)
		{dst: "169.254.1.1:0", from: "192.168.1.1:0", want: false},
		{dst: "[fe80::1]:0", from: "[fc00::1]:0", want: false},
		// link-local ↔ loopback (false)
		{dst: "169.254.1.1:0", from: "127.0.0.1:0", want: false},
		// routed ↔ routed: private ↔ private (true)
		{dst: "192.168.1.5:0", from: "192.168.1.1:0", want: true},
		{dst: "10.0.0.1:0", from: "172.16.0.1:0", want: true},
		{dst: "[fc00::1]:0", from: "[fd00::1]:0", want: true},
		// routed ↔ routed: private ↔ public (true — NAT/routing)
		{dst: "8.8.8.8:0", from: "192.168.1.1:0", want: true},
		{dst: "192.168.1.1:0", from: "8.8.8.8:0", want: true},
		// routed ↔ routed: public ↔ public (true)
		{dst: "8.8.8.8:0", from: "1.1.1.1:0", want: true},
		{dst: "[2001:db8::1]:0", from: "[2001:db8::2]:0", want: true},
		// public ↔ loopback (false)
		{dst: "[2001:db8::1]:0", from: "[::1]:0", want: false},
		// cross-family: loopback ↔ IPv4 (false)
		{dst: "127.0.0.1:0", from: "[::1]:0", want: false},
		{dst: "[::1]:0", from: "127.0.0.1:0", want: false},
		// cross-family: link-local ↔ IPv4 (false)
		{dst: "169.254.1.1:0", from: "[fe80::1]:0", want: false},
		{dst: "[fe80::1]:0", from: "169.254.1.1:0", want: false},
		// cross-family: routed public IPv4 from routed IPv6 (false)
		{dst: "8.8.8.8:0", from: "[2001:db8::1]:0", want: false},
		{dst: "1.1.1.1:0", from: "[2001:db8::2]:0", want: false},
		// cross-family: routed private IPv4 from routed IPv6 (false)
		{dst: "192.168.1.1:0", from: "[2001:db8::1]:0", want: false},
		{dst: "10.0.0.1:0", from: "[fd00::1]:0", want: false},
		{dst: "192.168.1.1:0", from: "[fc00::1]:0", want: false},
		// cross-family: routed private IPv4 from routed public IPv6 (false)
		{dst: "192.168.1.1:0", from: "[2001:db8::1]:0", want: false},
		{dst: "10.0.0.1:0", from: "[2001:db8::1]:0", want: false},
		// cross-family: routed public IPv4 from routed private IPv6 (false)
		{dst: "8.8.8.8:0", from: "[fc00::1]:0", want: false},
		{dst: "1.1.1.1:0", from: "[fd00::1]:0", want: false},
		// IPv4-in-IPv6 treated as IPv4
		{dst: "[::ffff:127.0.0.1]:0", from: "127.0.0.1:0", want: true},
		{dst: "[::ffff:8.8.8.8]:0", from: "192.168.1.1:0", want: true},
	} {
		dst := netip.MustParseAddrPort(tc.dst)
		from := netip.MustParseAddrPort(tc.from)
		got := netx.Reachable(dst, from)
		require.Equalf(t, tc.want, got, "Reachable(%s, %s)", tc.dst, tc.from)
	}
}
