package dht

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func backgroundServe(t *testing.T, s *Server, pc net.PacketConn) Binding {
	b, err := s.ServeBinding(t.Context(), pc)
	require.NoError(t, err)
	return b
}
