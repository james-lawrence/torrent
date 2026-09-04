package int160_test

import (
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func TestRangerDynamic(t *testing.T) {
	t.Run("derives its stable suffix from the server id", func(t *testing.T) {
		s, err := dht.NewServer(32)
		require.NoError(t, err)
		t.Cleanup(s.Close)

		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, s.Serve(t.Context(), pc))

		id := s.ID(s.DynamicAddrPort())

		r := int160.NewRangeDynamic(s, 16)

		require.Equal(t, int160.StableSuffix(id), r.Stable())
		require.Len(t, r.Ranges(), 16)
	})

	t.Run("Generate preserves the stable suffix of the server id", func(t *testing.T) {
		s, err := dht.NewServer(32)
		require.NoError(t, err)
		t.Cleanup(s.Close)

		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, s.Serve(t.Context(), pc))

		id := s.ID(s.DynamicAddrPort())

		r := int160.NewRangeDynamic(s, 16)
		generated := r.Generate()

		require.Equal(t, id.Bytes()[3:], generated.Bytes()[3:])
		require.Equal(t, byte(0x00), generated.Bytes()[2]&0x07)
	})

	t.Run("successive calls produce distinct values", func(t *testing.T) {
		s, err := dht.NewServer(32)
		require.NoError(t, err)
		t.Cleanup(s.Close)

		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, s.Serve(t.Context(), pc))

		r := int160.NewRangeDynamic(s, 1024)

		require.Eventually(t, func() bool {
			a := r.Generate()
			b := r.Generate()
			return a != b
		}, time.Second, 10*time.Millisecond)
	})
}
