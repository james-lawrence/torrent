package dht

import (
	"math/rand/v2"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func randomNode() *node {
	return &node{
		nodeKey: nodeKey{
			Id: int160.Random(),
			Addr: NewAddr(
				netip.AddrPortFrom(
					netip.AddrFrom4([4]byte{127, 0, 0, byte(rand.UintN(256))}),
					10000,
				),
			),
		},
	}
}

func TestBucket(t *testing.T) {
	t.Run("should iterate over all nodes", func(t *testing.T) {
		b := bucket{}

		for range 256 {
			b.AddNode(randomNode(), 0)
		}

		var c uint16
		ret := b.EachNode(func(n *node) bool {
			c++
			return true
		})

		require.EqualValues(t, 256, c)
		require.True(t, ret)
	})
}
