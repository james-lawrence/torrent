package netx

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeAddr struct {
	network string
	str     string
}

func (f fakeAddr) Network() string { return f.network }
func (f fakeAddr) String() string  { return f.str }

func TestComputeBestAddr(t *testing.T) {
	t.Run("SpecificAddr", func(t *testing.T) {
		cases := []struct {
			name string
			addr net.Addr
			want netip.AddrPort
		}{
			{
				name: "ipv4",
				addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.42"), Port: 9000},
				want: netip.MustParseAddrPort("192.168.1.42:9000"),
			},
			{
				name: "ipv6 global unicast",
				addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 9000},
				want: netip.MustParseAddrPort("[2001:db8::1]:9000"),
			},
			{
				name: "ipv6 /64 host",
				addr: &net.TCPAddr{IP: net.ParseIP("2001:db8:dead:beef:1:2:3:4"), Port: 9000},
				want: netip.MustParseAddrPort("[2001:db8:dead:beef:1:2:3:4]:9000"),
			},
			{
				name: "ipv6 link-local",
				addr: &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 9000},
				want: netip.MustParseAddrPort("[fe80::1]:9000"),
			},
			{
				name: "ipv6 ULA",
				addr: &net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 9000},
				want: netip.MustParseAddrPort("[fd00::1]:9000"),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				best := ComputeBestAddr(tc.addr)
				require.Equal(t, tc.want, best)
			})
		}
	})

	t.Run("UnparsableAddr", func(t *testing.T) {
		// An addr whose string form cannot be parsed returns the zero AddrPort.
		best := ComputeBestAddr(fakeAddr{network: "tcp", str: "not-an-addr"})
		require.False(t, best.IsValid())
	})

	t.Run("UnspecifiedV4", func(t *testing.T) {
		// When bound to 0.0.0.0, the function should enumerate interfaces and
		// return a routable address (not loopback, not unspecified).
		addr := &net.UDPAddr{IP: net.IPv4zero, Port: 7777}
		got := ComputeBestAddr(addr)

		if !got.IsValid() {
			t.Skip("no valid interface addresses available in test environment")
		}

		require.EqualValues(t, 7777, got.Port(), "port mismatch")
		require.False(t, got.Addr().IsUnspecified(), "expected a specific address, got unspecified: %s", got)
		require.False(t, got.Addr().Is6(), "expected an IPv4 address for IPv4 listener, got: %s", got)
	})

	t.Run("UnspecifiedV6", func(t *testing.T) {
		// Same as above but for ::
		addr := &net.UDPAddr{IP: net.IPv6unspecified, Port: 4444}
		got := ComputeBestAddr(addr)

		if !got.IsValid() {
			t.Skip("no valid interface addresses available in test environment")
		}

		require.EqualValues(t, 4444, got.Port(), "port mismatch")
		require.False(t, got.Addr().IsUnspecified(), "expected a specific address, got unspecified: %s", got)
		require.True(t, got.Addr().Is6(), "expected an IPv6 address for IPv6 listener, got: %s", got)
	})

	t.Run("PortPreserved", func(t *testing.T) {
		// The port from the bound address must be preserved in the result.
		const wantPort = 12345
		addr := &net.TCPAddr{IP: net.IPv4zero, Port: wantPort}
		got := ComputeBestAddr(addr)
		require.Equal(t, wantPort, int(got.Port()), "port not preserved")
	})
}

func TestComputeBestAddr4(t *testing.T) {
	t.Run("SpecificAddr", func(t *testing.T) {
		cases := []struct {
			name string
			addr net.Addr
			want netip.AddrPort
		}{
			{
				name: "ipv4",
				addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.42"), Port: 9000},
				want: netip.MustParseAddrPort("192.168.1.42:9000"),
			},
			{
				name: "ipv6 returns unchanged not converted to IPv4",
				addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 9000},
				want: netip.MustParseAddrPort("[2001:db8::1]:9000"),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				best := ComputeBestAddr4(tc.addr)
				require.Equal(t, tc.want, best)
			})
		}
	})

	t.Run("UnparsableAddr", func(t *testing.T) {
		best := ComputeBestAddr4(fakeAddr{network: "tcp", str: "not-an-addr"})
		require.False(t, best.IsValid())
	})

	t.Run("UnspecifiedV4", func(t *testing.T) {
		addr := &net.UDPAddr{IP: net.IPv4zero, Port: 8888}
		got := ComputeBestAddr4(addr)

		if !got.IsValid() {
			t.Skip("no valid interface addresses available in test environment")
		}

		require.EqualValues(t, 8888, got.Port(), "port mismatch")
		require.True(t, got.Addr().Is4(), "expected IPv4 address, got: %s", got)
	})

	t.Run("UnspecifiedV6", func(t *testing.T) {
		// When bound to ::, ComputeBestAddr4 should still return an IPv4 address
		// (unlike ComputeBestAddr which preserves the address family).
		addr := &net.UDPAddr{IP: net.IPv6unspecified, Port: 5555}
		got := ComputeBestAddr4(addr)

		if !got.IsValid() {
			t.Skip("no valid interface addresses available in test environment")
		}

		require.EqualValues(t, 5555, got.Port(), "port mismatch")
		require.True(t, got.Addr().Is4(), "expected IPv4 address for :: bound listener, got: %s", got)
	})

	t.Run("PortPreserved", func(t *testing.T) {
		const wantPort = 31337
		addr := &net.TCPAddr{IP: net.IPv4zero, Port: wantPort}
		got := ComputeBestAddr4(addr)
		require.Equal(t, wantPort, int(got.Port()), "port not preserved")
	})
}
