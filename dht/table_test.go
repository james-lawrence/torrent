package dht

import (
	"net/netip"
	"sync"
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

// TestTableAddDropRace reproduces the panic "missing id for addr" from
// server.addNode: concurrent goroutines racing to fill/evict the same bucket
// expose two bugs -- dropNode reads tbl.addrs without tbl.m (data race), and
// addNode makes the node visible in the bucket before tbl.addrs is populated
// (panic window). Run with -race to surface the data race.
func TestTableAddDropRace(t *testing.T) {
	const k = 4
	root := int160.Zero()
	tbl := newTable(k)

	// simulateServerAddNode mirrors the logic in Server.addNode.
	simulateServerAddNode := func(n *node) {
		b := tbl.bucketForID(root, n.Id)
		if excess := tbl.dropN(root, b, max(0, b.Len()-tbl.k)); excess >= 0 {
			return
		}
		tbl.addNode(root, n) //nolint:errcheck
	}

	var wg sync.WaitGroup
	for range 64 {
		n := randomNode()
		wg.Add(1)
		go func() {
			defer wg.Done()
			simulateServerAddNode(n)
		}()
	}
	wg.Wait()
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
