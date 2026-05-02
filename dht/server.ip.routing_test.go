package dht

import (
	"net"
	"net/netip"
	"testing"

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
	backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))

	require.Equal(t, 2, s.numBindings())
}

func TestServe_BindingIDPerSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("requires IPv6 loopback")
	}
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b6 := backgroundServe(t, s, mustListenUDP(t, "[::1]:0"))

	require.NotEqual(t, b4.ID(), b6.ID())
}

func TestAddrPort_SelectsBySourceFamily(t *testing.T) {
	s, err := NewServer(8)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	b4 := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))
	b4b := backgroundServe(t, s, mustListenUDP(t, "127.0.0.1:0"))

	src4 := netip.MustParseAddrPort("1.2.3.4:6881")
	got := s.AddrPort(src4)
	require.True(t, got.Addr().Is4() || got.Addr().Is4In6())

	addr4 := b4.AddrPort()
	addr4b := b4b.AddrPort()
	require.True(t, got == addr4 || got == addr4b)
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
