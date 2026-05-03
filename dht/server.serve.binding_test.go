package dht

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServeBinding(t *testing.T) {
	t.Run("does not bind same listener twice", func(t *testing.T) {
		s := mustNewServer(t)
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		b1, err := s.ServeBinding(t.Context(), pc)
		require.NoError(t, err)
		b2, err := s.ServeBinding(t.Context(), pc)
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

		b1, err := s.ServeBinding(t.Context(), pc1)
		require.NoError(t, err)
		b2, err := s.ServeBinding(t.Context(), pc2)
		require.NoError(t, err)
		require.Equal(t, 2, s.numBindings())
		require.NotEqual(t, b1.AddrPort(), b2.AddrPort())
	})
}
