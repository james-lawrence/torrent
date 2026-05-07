package dht

import (
	"net"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func TestDynamicAddrPort(t *testing.T) {
	t.Run("nil server returns unspecified", func(t *testing.T) {
		var nilS *Server
		got := nilS.DynamicAddrPort()
		require.Equal(t, netip.AddrPortFrom(netip.IPv6Unspecified(), 0), got)
	})

	t.Run("server with no bindings returns unspecified", func(t *testing.T) {
		s := mustNewServer(t)
		got := s.DynamicAddrPort()
		require.Equal(t, netip.AddrPortFrom(netip.IPv6Unspecified(), 0), got)
	})

	t.Run("server with binding returns detected address", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		got := s.DynamicAddrPort()
		require.True(t, got.IsValid(), "expected valid AddrPort, got %s", got)
		require.True(t, got.Addr().Is4(), "expected IPv4, got %s", got)
	})

	t.Run("ID returns zero when dynamicaddrport is unspecified", func(t *testing.T) {
		s := mustNewServer(t)
		dap := s.DynamicAddrPort()
		require.True(t, dap.Addr().IsUnspecified())
		require.Equal(t, int160.Zero(), s.ID(dap))
	})

	t.Run("ID returns binding id after dynamicaddrport resolves", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		dap := s.DynamicAddrPort()
		require.True(t, dap.IsValid())
		require.True(t, dap.Addr().Is4())
		got := s.ID(dap)
		require.NotEqual(t, int160.Zero(), got, "ID(dap) should return the binding's node ID")
	})
}
