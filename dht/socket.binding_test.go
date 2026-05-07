package dht

import (
	"net"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func TestUpdateNodeRejectsUnreachableAddress(t *testing.T) {
	id := krpc.ID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

	t.Run("ipv4 loopback binding rejects routed address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("192.168.1.1:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})

	t.Run("ipv6 loopback binding rejects routed address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("[2a00:1370:81ac:820:4dea:ca75:322:3d54]:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})

	t.Run("ipv4 binding rejects ipv6 address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("[2001:db8::1]:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})

	t.Run("ipv6 binding rejects ipv4 address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("192.168.1.1:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})

	t.Run("ipv4 binding rejects ipv6 loopback address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("[::1]:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})

	t.Run("ipv6 binding rejects ipv4 loopback address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		binding := b.(*socketbinding)
		err = binding.updateNode(
			NewAddr(netip.MustParseAddrPort("127.0.0.1:12345")),
			&id,
			false,
			func(*node) {},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "node not allowed in this table")
	})
}
