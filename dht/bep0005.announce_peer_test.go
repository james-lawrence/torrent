package dht

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

// mockUDPSocket is a UDP socket that captures outgoing packets for inspection.
// It listens on a real UDP port so the DHT code can send to it, and provides
// methods to read captured messages as raw []byte.
type mockUDPSocket struct {
	conn *net.UDPConn
}

func newMockUDPSocket(t *testing.T, network string, addr string) *mockUDPSocket {
	t.Helper()
	resolved, err := net.ResolveUDPAddr(network, addr)
	require.NoError(t, err)
	conn, err := net.ListenUDP(network, resolved)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &mockUDPSocket{conn: conn}
}

func (m *mockUDPSocket) Addr() string {
	return m.conn.LocalAddr().String()
}

// ReadOne reads a single packet from the socket, blocking up to 2 seconds.
// Returns the raw bytes and the source address.
func (m *mockUDPSocket) ReadOne(t *testing.T) ([]byte, netip.AddrPort) {
	t.Helper()
	b := make([]byte, 0x10000)
	_ = m.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, addr, err := m.conn.ReadFromUDP(b)
	require.NoError(t, err)
	_ = addr // ReadFromUDP always returns *net.UDPAddr
	return b[:n], netip.AddrPortFrom(netip.AddrFrom16([16]byte(addr.IP.To16())), uint16(addr.Port))
}

// ReadOneTo reads a single packet and verifies it came from the expected address.
func (m *mockUDPSocket) ReadOneTo(t *testing.T, expected netip.AddrPort) ([]byte, netip.AddrPort) {
	t.Helper()
	b, from := m.ReadOne(t)
	require.Equal(t, expected, from, "unexpected source address")
	return b, from
}

func TestAnnouncePeer(t *testing.T) {
	t.Skip()

	t.Run("binding_selection_ipv4_destination_with_ipv4_binding", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		got := s.ID(netip.MustParseAddrPort("127.0.0.1:6881"))
		require.Equal(t, s.Binding(netip.MustParseAddrPort("127.0.0.1:6881")).ID(), got, "expected IPv4 binding's ID")
	})

	t.Run("binding_selection_ipv6_destination_with_ipv6_binding", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		got := s.ID(netip.MustParseAddrPort("[::1]:6881"))
		require.Equal(t, s.Binding(netip.MustParseAddrPort("[::1]:6881")).ID(), got, "expected IPv6 binding's ID")
	})

	t.Run("binding_selection_ipv4_destination_with_both_bindings", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		ipv4Binding, err := s.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ipv4PC.Close() })

		ipv6PC, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), ipv6PC, netx.ComputeBestAddr(ipv6PC.LocalAddr()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ipv6PC.Close() })

		got := s.ID(netip.MustParseAddrPort("127.0.0.1:6881"))
		require.Equal(t, ipv4Binding.(*socketbinding).ID(), got, "expected IPv4 binding's ID")
	})

	t.Run("binding_selection_ipv6_destination_with_both_bindings", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ipv4PC.Close() })

		ipv6PC, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		ipv6Binding, err := s.ServeBinding(t.Context(), ipv6PC, netx.ComputeBestAddr(ipv6PC.LocalAddr()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = ipv6PC.Close() })

		got := s.ID(netip.MustParseAddrPort("[::1]:6881"))
		require.Equal(t, ipv6Binding.(*socketbinding).ID(), got, "expected IPv6 binding's ID")
	})

	t.Run("binding_selection_ipv4_destination_with_routed_binding", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		got := s.ID(netip.MustParseAddrPort("192.168.1.1:6881"))
		require.Equal(t, s.Binding(netip.MustParseAddrPort("192.168.1.1:6881")).ID(), got, "expected IPv4 binding's ID")
	})

	t.Run("binding_selection_ipv6_destination_with_routed_binding", func(t *testing.T) {
		wantID := int160.Random()
		s := mustNewServer(t, OptionNodeID(wantID))

		pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::"), Port: 0})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pc.Close() })

		_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
		require.NoError(t, err)

		got := s.ID(netip.MustParseAddrPort("[2001:db8::1]:6881"))
		require.Equal(t, s.Binding(netip.MustParseAddrPort("[2001:db8::1]:6881")).ID(), got, "expected IPv6 binding's ID")
	})

	t.Run("multi_binding_packet_routing_ipv4_destination_uses_ipv4_binding", func(t *testing.T) {
		serverID := int160.Random()

		// Server A: multi-binding with IPv4 + IPv6
		sA, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { sA.Close() })

		// IPv4 listener for Server A
		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = sA.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)

		// IPv6 listener for Server A
		ipv6PC, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		_, err = sA.ServeBinding(t.Context(), ipv6PC, netx.ComputeBestAddr(ipv6PC.LocalAddr()))
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = ipv4PC.Close()
			_ = ipv6PC.Close()
		})

		// Destination sockets that the DHT code will send announce_peer to.
		// These act as a simple UDP server for each test.
		mockIPv4Socket := newMockUDPSocket(t, "udp4", "127.0.0.1:0")

		infoHash := int160.Random()
		token := "test-token"

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		ipv4Addr := NewAddr(netip.MustParseAddrPort(mockIPv4Socket.Addr()))
		done := make(chan struct{})
		go func() {
			sA.announcePeer(ctx, ipv4Addr, infoHash, 6881, token, false)
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("announcePeer timed out for IPv4")
		}

		captured, _ := mockIPv4Socket.ReadOne(t)
		require.NotEmpty(t, captured, "expected packet on IPv4 mock listener")

		var msg krpc.Msg
		require.NoError(t, bencode.Unmarshal(captured, &msg))
		require.Equal(t, "q", msg.Y)
		require.Equal(t, "announce_peer", msg.Q)
		require.Equal(t, krpc.ID(serverID.Bytes()), msg.A.ID, "expected server's IPv4 binding node ID in request")
	})

	t.Run("multi_binding_packet_routing_ipv6_destination_uses_ipv6_binding", func(t *testing.T) {
		serverID := int160.Random()

		// Server A: multi-binding with IPv4 + IPv6
		sA, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { sA.Close() })

		// IPv4 listener for Server A
		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = sA.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)

		// IPv6 listener for Server A
		ipv6PC, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		_, err = sA.ServeBinding(t.Context(), ipv6PC, netx.ComputeBestAddr(ipv6PC.LocalAddr()))
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = ipv4PC.Close()
			_ = ipv6PC.Close()
		})

		// Destination socket that the DHT code will send announce_peer to.
		mockIPv6Socket := newMockUDPSocket(t, "udp6", "[::1]:0")

		infoHash := int160.Random()
		token := "test-token"

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		ipv6Addr := NewAddr(netip.MustParseAddrPort(mockIPv6Socket.Addr()))

		done := make(chan struct{})
		go func() {
			sA.announcePeer(ctx, ipv6Addr, infoHash, 6881, token, false)
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("announcePeer timed out for IPv6")
		}

		captured, _ := mockIPv6Socket.ReadOne(t)
		require.NotEmpty(t, captured, "expected packet on IPv6 mock listener")

		var msg krpc.Msg
		require.NoError(t, bencode.Unmarshal(captured, &msg))
		require.Equal(t, "q", msg.Y)
		require.Equal(t, "announce_peer", msg.Q)
		require.Equal(t, krpc.ID(serverID.Bytes()), msg.A.ID, "expected server's IPv6 binding node ID in request")
	})

	t.Run("request_content", func(t *testing.T) {
		serverID := int160.Random()
		infoHash := int160.Random()
		port := uint16(6881)
		token := "test-token"
		impliedPort := false

		// Server A: single IPv4 binding
		sA, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { sA.Close() })

		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = sA.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)

		// Destination socket that the DHT code will send announce_peer to.
		mockSocket := newMockUDPSocket(t, "udp4", "127.0.0.1:0")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		destination := NewAddr(netip.MustParseAddrPort(mockSocket.Addr()))

		done := make(chan struct{})
		go func() {
			sA.announcePeer(ctx, destination, infoHash, port, token, impliedPort)
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("announcePeer timed out")
		}

		captured, _ := mockSocket.ReadOne(t)
		require.NotEmpty(t, captured, "expected captured packet")

		var msg krpc.Msg
		require.NoError(t, bencode.Unmarshal(captured, &msg))
		require.Equal(t, "q", msg.Y)
		require.Equal(t, "announce_peer", msg.Q)
		require.NotNil(t, msg.A)
		require.Equal(t, krpc.ID(serverID.Bytes()), msg.A.ID)
		require.Equal(t, krpc.ID(infoHash.Bytes()), msg.A.InfoHash)
		require.NotNil(t, msg.A.Port)
		require.Equal(t, port, *msg.A.Port)
		require.Equal(t, token, msg.A.Token)
		require.Equal(t, impliedPort, msg.A.ImpliedPort)
	})

	t.Run("single_binding_ipv4_only", func(t *testing.T) {
		serverID := int160.Random()

		s, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })

		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)

		// Verify binding selection works for IPv4
		id := s.ID(netip.MustParseAddrPort("127.0.0.1:6881"))
		require.NotEqual(t, int160.Zero(), id)
		require.Equal(t, s.Binding(netip.MustParseAddrPort("127.0.0.1:6881")).ID(), id)
	})

	t.Run("single_binding_ipv6_only", func(t *testing.T) {
		serverID := int160.Random()

		s, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })

		ipv6PC, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0})
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), ipv6PC, netx.ComputeBestAddr(ipv6PC.LocalAddr()))
		require.NoError(t, err)

		// Verify binding selection works for IPv6
		id := s.ID(netip.MustParseAddrPort("[::1]:6881"))
		require.NotEqual(t, int160.Zero(), id)
		require.Equal(t, s.Binding(netip.MustParseAddrPort("[::1]:6881")).ID(), id)
	})

	t.Run("no_matching_binding", func(t *testing.T) {
		serverID := int160.Random()

		s, err := NewServer(32, OptionNodeID(serverID))
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })

		// Only IPv4 binding
		ipv4PC, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)
		_, err = s.ServeBinding(t.Context(), ipv4PC, netx.ComputeBestAddr(ipv4PC.LocalAddr()))
		require.NoError(t, err)

		// IPv6 destination should get zero ID from binding selection
		ipv6Addr := netip.MustParseAddrPort("[::1]:6881")
		id := s.ID(ipv6Addr)
		require.Equal(t, int160.Zero(), id, "expected zero ID when no binding can reach destination")

		// announcePeer with zero ID: NewAnnouncePeerRequest accepts zero ID
		// but the request will carry a zero node ID
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		destination := NewAddr(ipv6Addr)
		done := make(chan struct{})
		go func() {
			s.announcePeer(ctx, destination, int160.Random(), 6881, "token", false)
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			// Timeout is acceptable - the query will fail to get a response
			// since there's no matching binding and no mock server listening
		}
	})
}
