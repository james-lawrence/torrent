package dht

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/dht/int160"
)

func TestTable(t *testing.T) {
	t.Run("add remove nodes", func(t *testing.T) {
		root := int160.Zero()
		tbl := newTable(8)
		assert.Equal(t, 0, tbl.bucketIndex(root, int160.Max()))
		assert.Panics(t, func() { tbl.bucketIndex(root, int160.Zero()) }, "root node does not belong in a bucket")

		assert.Error(t, tbl.addNode(root, &node{}))
		assert.Equal(t, 0, tbl.buckets[0].Len())

		id0 := int160.FromByteString("\x2f\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
		id1 := int160.FromByteString("\x2e\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
		n0 := &node{nodeKey: nodeKey{
			Id:   id0,
			Addr: NewAddr(netip.AddrPort{}),
		}}
		n1 := &node{nodeKey: nodeKey{
			Id:   id1,
			Addr: NewAddr(netip.AddrPort{}),
		}}

		assert.NoError(t, tbl.addNode(root, n0))
		assert.Equal(t, 1, tbl.buckets[2].Len())

		assert.Error(t, tbl.addNode(root, n0))
		assert.Equal(t, 1, tbl.buckets[2].Len())
		assert.Equal(t, 1, tbl.numNodes())

		assert.NoError(t, tbl.addNode(root, n1))
		assert.Equal(t, 2, tbl.buckets[2].Len())
		assert.Equal(t, 2, tbl.numNodes())

		tbl.dropNode(root, n0)
		assert.Equal(t, 1, tbl.buckets[2].Len())
		assert.Equal(t, 1, tbl.numNodes())

		tbl.dropNode(root, n1)
		assert.Equal(t, 0, tbl.buckets[2].Len())
		assert.Equal(t, 0, tbl.numNodes())
	})

	t.Run("iterate over all nodes", func(t *testing.T) {
		const n = 256
		root := int160.Zero()
		tbl := newTable(8)
		assert.Equal(t, 0, tbl.bucketIndex(root, int160.Max()))

		v := uint(0)
		for range n {
			if err := tbl.addNode(root, randomNode()); err == nil {
				v++
			}
		}

		var c uint
		ret := tbl.forNodes(func(n *node) bool {
			c++
			return true
		})
		require.True(t, ret)
		require.EqualValues(t, v, c)
	})
}

func TestRandomIdInBucket(t *testing.T) {
	root := int160.Random()
	tbl := table{}
	t.Logf("%v: table root id", root)
	for i := range tbl.buckets {
		id := tbl.randomIdForBucket(root, i)
		t.Logf("%v: random id for bucket index %v", id, i)
		assert.Equal(t, tbl.bucketIndex(root, id), i)
	}
}
