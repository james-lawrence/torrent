package dht

import (
	"context"
	"iter"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func TestServeBinding(t *testing.T) {
	t.Run("does not bind same listener twice", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b1, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)
		b2, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)
		require.Equal(t, 1, s.numBindings())
		require.Same(t, b1, b2)
	})

	t.Run("allows different listeners", func(t *testing.T) {
		s := mustNewServer(t)
		pc1, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		pc2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = pc1.Close()
			_ = pc2.Close()
		})

		b1, err := s.ServeBinding(t.Context(), pc1, netx.ComputeBestAddr(pc1.LocalAddr()))
		require.NoError(t, err)
		b2, err := s.ServeBinding(t.Context(), pc2, netx.ComputeBestAddr(pc2.LocalAddr()))
		require.NoError(t, err)
		require.Equal(t, 2, s.numBindings())
		require.NotEqual(t, b1.AddrPort(), b2.AddrPort())
	})

	t.Run("stats reports the bound addresses", func(t *testing.T) {
		s := mustNewServer(t)
		pc1, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		pc2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = pc1.Close()
			_ = pc2.Close()
		})

		_, err = s.ServeBinding(t.Context(), pc1, netx.ComputeBestAddr(pc1.LocalAddr()))
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), pc2, netx.ComputeBestAddr(pc2.LocalAddr()))
		require.NoError(t, err)

		ap1, err := netx.AddrPort(pc1.LocalAddr())
		require.NoError(t, err)
		ap2, err := netx.AddrPort(pc2.LocalAddr())
		require.NoError(t, err)

		bound := s.Stats().BoundAddrs
		require.Len(t, bound, 2)
		require.Contains(t, bound, ap1)
		require.Contains(t, bound, ap2)
	})
}

func TestServe(t *testing.T) {
	t.Run("ipv4 socket creates a single binding", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		err = s.Serve(t.Context(), pc)
		require.NoError(t, err)
		require.Equal(t, 1, s.numBindings())
	})

	t.Run("ipv6 socket creates at least two bindings (ipv6 + ipv4)", func(t *testing.T) {
		// Exact count is host-dependent: a wildcard bind now registers one
		// binding per (scope, family) group actually present (loopback,
		// link-local, routed), so a multi-homed host produces more than the
		// routed-scope minimum of two.
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		err = s.Serve(t.Context(), pc)
		require.NoError(t, err)
		require.GreaterOrEqual(t, s.numBindings(), 2)

		require.True(t, s.AddrPort(netip.MustParseAddrPort("8.8.8.8:12345")).Addr().Is4())
		require.True(t, s.AddrPort(netip.MustParseAddrPort("[2a00:1370:81ac:820:4dea:ca75:322:3d54]:28935")).Addr().Is6())
	})

	t.Run("ipv6 socket with failed ipv4 binding still succeeds", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		err = s.Serve(t.Context(), pc)
		require.NoError(t, err)
		require.GreaterOrEqual(t, s.numBindings(), 1)
	})

	t.Run("wildcard socket must be reachable via its own loopback address", func(t *testing.T) {
		// A socket bound to the wildcard address accepts traffic on every
		// local address, loopback included - so a peer connecting to it over
		// loopback must be recognized as reaching this binding. ":0" on
		// network "udp" is the exact form a dual-stack wildcard bind takes in
		// practice, letting the OS/Go pick the address, with no synthetic
		// override of the computed best address.
		s := mustNewServer(t)
		pc, err := net.ListenPacket("udp", ":0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		err = s.Serve(t.Context(), pc)
		require.NoError(t, err)

		got := s.ID(netip.MustParseAddrPort("127.0.0.1:12345"))
		require.NotEqual(t, int160.Zero(), got, "wildcard-bound socket must recognize its own loopback address as reachable")
	})

	t.Run("wildcard socket resolves its per-group bindings concurrently", func(t *testing.T) {
		// serveBinding blocks until its address is resolved (e.g. a real
		// resolvepublicaddr doing UPnP discovery/port-mapping I/O), and a
		// wildcard bind registers one binding per (scope, family) group
		// present on the host. If Serve resolved those groups one at a time,
		// its total latency would be the sum of every group's resolution
		// delay instead of the slowest one - assert it isn't.
		const delay = 200 * time.Millisecond

		slow := func(ctx context.Context, sc *Server, q Binding, id int160.T, bestaddr netip.AddrPort, local net.PacketConn) (iter.Seq[netip.AddrPort], error) {
			time.Sleep(delay)
			return func(yield func(netip.AddrPort) bool) { yield(bestaddr) }, nil
		}

		s := mustNewServer(t, OptionDynamicPort(slow))
		pc, err := net.ListenPacket("udp", ":0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		groups := len(netx.ComputeReachableAddrs(pc.LocalAddr()))
		require.Greater(t, groups, 1, "test host needs more than one reachable group to prove concurrency")

		start := time.Now()
		err = s.Serve(t.Context(), pc)
		elapsed := time.Since(start)
		require.NoError(t, err)

		require.Less(t, elapsed, time.Duration(groups)*delay, "Serve took as long as resolving every group sequentially")
	})
}
