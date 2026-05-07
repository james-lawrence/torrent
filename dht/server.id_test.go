package dht

import (
	"net"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/stretchr/testify/require"
)

func mustNewServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	s, err := NewServer(20, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func backgroundServe(t *testing.T, s *Server, pc net.PacketConn) Binding {
	b, err := s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
	require.NoError(t, err)
	return b
}

func TestServerID(t *testing.T) {
	wantID := int160.FromByteString("03" + string(make([]byte, 19, 20)))

	cases := []struct {
		name      string
		setup     func(t *testing.T) *Server
		source    netip.AddrPort
		wantValid bool
	}{
		{
			name:      "no_bindings",
			setup:     func(t *testing.T) *Server { return mustNewServer(t, OptionNodeID(wantID)) },
			source:    netip.MustParseAddrPort("127.0.0.1:12345"),
			wantValid: false,
		},
		{
			name: "reachable_loopback",
			setup: func(t *testing.T) *Server {
				s := mustNewServer(t, OptionNodeID(wantID))
				pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
				require.NoError(t, err)
				t.Cleanup(func() { _ = pc.Close() })
				_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
				require.NoError(t, err)
				return s
			},
			source:    netip.MustParseAddrPort("127.0.0.1:54321"),
			wantValid: true,
		},
		{
			name: "unreachable_routed_source",
			setup: func(t *testing.T) *Server {
				s := mustNewServer(t, OptionNodeID(wantID))
				pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
				require.NoError(t, err)
				t.Cleanup(func() { _ = pc.Close() })
				_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
				require.NoError(t, err)
				return s
			},
			source:    netip.MustParseAddrPort("192.168.1.1:12345"),
			wantValid: false,
		},
		{
			name: "unreachable_ipv6_source",
			setup: func(t *testing.T) *Server {
				s := mustNewServer(t, OptionNodeID(wantID))
				pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
				require.NoError(t, err)
				t.Cleanup(func() { _ = pc.Close() })
				_, err = s.ServeBinding(t.Context(), pc, netx.ComputeBestAddr(pc.LocalAddr()))
				require.NoError(t, err)
				return s
			},
			source:    netip.MustParseAddrPort("[::1]:12345"),
			wantValid: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.setup(t)
			got := s.ID(tc.source)
			if tc.wantValid {
				require.NotEqual(t, int160.Zero(), got)
			} else {
				require.Equal(t, int160.Zero(), got)
			}
		})
	}
}
