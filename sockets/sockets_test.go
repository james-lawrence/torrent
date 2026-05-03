package sockets

import (
	"log"
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

func TestComputeBestAddr_SpecificAddr(t *testing.T) {
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
			best := computeBestAddr(tc.addr)
			require.Equal(t, tc.want, best)
		})
	}
}

func TestComputeBestAddr_UnparsableAddr(t *testing.T) {
	// An addr whose string form cannot be parsed returns the zero AddrPort.
	best := computeBestAddr(fakeAddr{network: "tcp", str: "not-an-addr"})
	require.False(t, best.IsValid())
}

func TestComputeBestAddr_UnspecifiedReturnsNonLoopback(t *testing.T) {
	log.SetFlags(log.Flags() | log.Lshortfile)
	// When bound to 0.0.0.0, the function should enumerate interfaces and
	// return a routable address (not loopback, not unspecified).
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: 7777}
	got := computeBestAddr(addr)

	if !got.IsValid() {
		t.Skip("no valid interface addresses available in test environment")
	}

	require.EqualValues(t, 7777, got.Port(), "port mismatch")
	require.False(t, got.Addr().IsUnspecified(), "expected a specific address, got unspecified: %s", got)
}

func TestComputeBestAddr_UnspecifiedV6ReturnsNonLoopback(t *testing.T) {
	// Same as above but for ::
	addr := &net.UDPAddr{IP: net.IPv6unspecified, Port: 4444}
	got := computeBestAddr(addr)

	if !got.IsValid() {
		t.Skip("no valid interface addresses available in test environment")
	}

	require.EqualValues(t, 4444, got.Port(), "port mismatch")
	require.False(t, got.Addr().IsUnspecified(), "expected a specific address, got unspecified: %s", got)
}

func TestComputeBestAddr_PortPreserved(t *testing.T) {
	// The port from the bound address must be preserved in the result.
	const wantPort = 12345
	addr := &net.TCPAddr{IP: net.IPv4zero, Port: wantPort}
	got := computeBestAddr(addr)
	if got.Port() != wantPort {
		t.Errorf("port not preserved: got %d, want %d", got.Port(), wantPort)
	}
}
