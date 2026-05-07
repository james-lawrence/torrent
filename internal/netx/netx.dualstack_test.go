//go:build !linux

package netx_test

import (
	"net"
	"testing"

	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func TestIsIPv6DualStack(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		require.False(t, netx.IsIPv6DualStack(nil))
	})

	t.Run("ipv6", func(t *testing.T) {
		conn, err := net.ListenPacket("udp6", "[::]:0")
		require.NoError(t, err)

		require.False(t, netx.IsIPv6DualStack(conn))
		conn.Close()
	})

	t.Run("ipv4", func(t *testing.T) {
		// udp4 is not an IPv6 socket, so GetsockoptInt will return
		// ENOPROTOOPT and the function should return false.
		ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		require.NoError(t, err)
		defer ln.Close()

		require.False(t, netx.IsIPv6DualStack(ln))
	})

	t.Run("closed UDP conn", func(t *testing.T) {
		ln, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
		require.NoError(t, err)
		ln.Close()

		require.False(t, netx.IsIPv6DualStack(ln))
	})
}
