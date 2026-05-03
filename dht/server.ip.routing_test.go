package dht

import (
	"net"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/stretchr/testify/require"
)

func mustListenUDP(t *testing.T, addr string) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	return pc
}

func TestServe_SingleSocket(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	require.False(t, b.ID().IsZero())
}

func TestServe_MultiSocket(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	backgroundServe(t, s, mustListenUDP(t, "127.0.0.2:0"))

	require.Equal(t, 2, s.numBindings())
}

func TestServe_BindingIDPerSocket(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	require.NotEqual(t, b4.ID(), b6.ID())
}

// TestAddrPort_SelectBinding verifies that AddrPort selects the binding whose
// local address is in the same CIDR scope as the source, falling back to
// same-family, then first binding.
func TestAddrPort_SelectBinding(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))

	// Loopback source → matches loopback IPv4 binding (same scope).
	got := s.AddrPort(netip.MustParseAddrPort("127.0.0.1:6881"))
	require.Equal(t, b4.AddrPort(), got)

	// Public IPv4 source → no scope match → zero AddrPort (no reachable binding).
	got = s.AddrPort(netip.MustParseAddrPort("8.8.8.8:6881"))
	require.Zero(t, got)
}

func TestAddrPort_SelectBinding_IPv6(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	// Loopback IPv4 source → IPv4 loopback binding.
	got4 := s.AddrPort(netip.MustParseAddrPort("127.0.0.1:6881"))
	require.Equal(t, b4.AddrPort(), got4)

	// Loopback IPv6 source → IPv6 loopback binding.
	got6 := s.AddrPort(netip.MustParseAddrPort("[::1]:6881"))
	require.Equal(t, b6.AddrPort(), got6)

	// Public IPv4 source → no scope match for loopback bindings → zero.
	require.Zero(t, s.AddrPort(netip.MustParseAddrPort("8.8.8.8:6881")))

	// Public IPv6 source → no scope match for loopback bindings → zero.
	require.Zero(t, s.AddrPort(netip.MustParseAddrPort("[2001:db8::1]:6881")))

	// IPv4-in-IPv6 source is unmapped to IPv4 → picks IPv4 binding.
	gotMapped := s.AddrPort(netip.MustParseAddrPort("[::ffff:127.0.0.1]:6881"))
	require.Equal(t, b4.AddrPort(), gotMapped)
}

// TestAddNode_RoutesIPv4ToIPv4Table verifies that an IPv4 node address is
// placed in the IPv4 binding's routing table and not the IPv6 binding's.
func TestAddNode_RoutesIPv4ToIPv4Table(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	ni := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.2:6881")),
	}
	require.NoError(t, s.AddNode(ni))

	require.Equal(t, 1, b4.Routing().numNodes())
	require.Equal(t, 0, b6.Routing().numNodes())
}

// TestAddNode_RoutesIPv6ToIPv6Table verifies that an IPv6 node address is
// placed in the IPv6 binding's routing table and not the IPv4 binding's.
func TestAddNode_RoutesIPv6ToIPv6Table(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	ni := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::1]:6881")),
	}
	require.NoError(t, s.AddNode(ni))

	require.Equal(t, 0, b4.Routing().numNodes())
	require.Equal(t, 1, b6.Routing().numNodes())
}

// TestAddNode_IPv4IPv6Split verifies that adding a mix of IPv4 and IPv6 nodes
// splits them across the correct binding tables.
func TestAddNode_IPv4IPv6Split(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	v4nodes := []krpc.NodeInfo{
		{ID: int160.Random().AsByteArray(), Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.2:6881"))},
		{ID: int160.Random().AsByteArray(), Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.3:6881"))},
	}
	v6nodes := []krpc.NodeInfo{
		{ID: int160.Random().AsByteArray(), Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::1]:6881"))},
		{ID: int160.Random().AsByteArray(), Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::1]:6882"))},
	}

	require.NoError(t, s.AddNode(append(v4nodes, v6nodes...)...))

	require.Equal(t, 2, b4.Routing().numNodes())
	require.Equal(t, 2, b6.Routing().numNodes())
}

// TestAddNode_NoBindingForScope verifies that nodes whose address has no
// matching binding scope are silently dropped.
func TestAddNode_NoBindingForScope(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))

	// Public IPv4 node — loopback binding can't reach it → not added.
	ni := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("8.8.8.8:6881")),
	}
	require.NoError(t, s.AddNode(ni))
	require.Equal(t, 0, b4.Routing().numNodes())
}

// TestSocketBinding_DynamicAddrEnforcesIPFamily verifies that a binding's
// dynamicaddr gates node addition to only accept addresses in the same IP
// family (IPv4 vs IPv6). The dynamicaddr is the binding's resolved public
// address, and bindingLocked uses netx.Reachable(addr, dynamicaddr) to find
// the right binding — Reachable returns false when the source and target
// addresses differ in IP family for loopback and link-local scopes.
func TestSocketBinding_DynamicAddrEnforcesIPFamily(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	// Verify the bindings have different IP families in their dynamicaddr.
	require.True(t, b4.AddrPort().Addr().Is4())
	require.True(t, b6.AddrPort().Addr().Is6())

	// An IPv4 node is only added to the IPv4 binding's table.
	ni4 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.10:6881")),
	}
	require.NoError(t, s.AddNode(ni4))
	require.Equal(t, 1, b4.Routing().numNodes())
	require.Equal(t, 0, b6.Routing().numNodes())

	// An IPv6 node is only added to the IPv6 binding's table.
	ni6 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::1]:6881")),
	}
	require.NoError(t, s.AddNode(ni6))
	require.Equal(t, 1, b4.Routing().numNodes())
	require.Equal(t, 1, b6.Routing().numNodes())

	// An IPv4-in-IPv6 node → Reachable() unmaps it → routed to IPv4 binding.
	ni4in6 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::ffff:127.0.0.10]:6881")),
	}
	require.NoError(t, s.AddNode(ni4in6))
	require.Equal(t, 2, b4.Routing().numNodes())
	require.Equal(t, 1, b6.Routing().numNodes())
}

// TestSocketBinding_IP6OnlyDynamicAddrEnforcesIPFamily verifies that an IPv6-only
// server rejects IPv4 (including IPv4-in-IPv6) nodes.
func TestSocketBinding_IP6OnlyDynamicAddrEnforcesIPFamily(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	// Verify the bindings have different IP families in their dynamicaddr.
	require.True(t, b6.AddrPort().Addr().Is6())

	// An IPv4 node is only added to the IPv4 binding's table.
	ni4 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.10:6881")),
	}
	require.NoError(t, s.AddNode(ni4))
	require.Equal(t, 0, b6.Routing().numNodes())

	// An IPv6 node is only added to the IPv6 binding's table.
	ni6 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::1]:6881")),
	}
	require.NoError(t, s.AddNode(ni6))
	require.Equal(t, 1, b6.Routing().numNodes())

	// An IPv4-in-IPv6 node → Reachable() unmaps it → no binding → not added.
	ni4in6 := krpc.NodeInfo{
		ID:   int160.Random().AsByteArray(),
		Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort("[::ffff:127.0.0.10]:6881")),
	}
	require.NoError(t, s.AddNode(ni4in6))
	require.Equal(t, 1, b6.Routing().numNodes())
}

func TestReply_UsesBindingID(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))

	qr := s.Ping(b.AddrPort())
	require.NoError(t, qr.ToError())
	require.NotNil(t, qr.Reply.R)
	require.NotNil(t, qr.Reply.SenderID())
}
