package dht

import (
	"net"
	"net/netip"
	"testing"

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

	t.Run("ipv6 socket creates two bindings (ipv6 + ipv4)", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		err = s.Serve(t.Context(), pc)
		require.NoError(t, err)
		require.Equal(t, 2, s.numBindings())

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
}
