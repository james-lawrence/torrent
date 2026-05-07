package dht

import (
	"errors"
	"fmt"
	"iter"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
)

func newTable(k int) *table {
	return &table{
		k: k,
		m: &sync.Mutex{},
	}
}

// Node table, with indexes on distance from root ID to bucket, and node addr.
type table struct {
	k       int
	m       *sync.Mutex
	buckets [160]bucket
	addrs   map[netip.AddrPort]map[int160.T]struct{}
}

func (tbl *table) K() int {
	return tbl.k
}

func (tbl *table) randomIdForBucket(root int160.T, bucketIndex int) int160.T {
	randomId := randomIdInBucket(root, bucketIndex)
	if randomIdBucketIndex := tbl.bucketIndex(root, randomId); randomIdBucketIndex != bucketIndex {
		panic(fmt.Sprintf("bucket index for random id %v == %v not %v", randomId, randomIdBucketIndex, bucketIndex))
	}
	return randomId
}

// dropN removes up to n nodes from b, evicting them from both the bucket and
// the address index atomically under tbl.m.
func (tbl *table) dropN(root int160.T, b *bucket, n int) int {
	if n == 0 {
		return b.Len() - tbl.k
	}

	tbl.m.Lock()
	defer tbl.m.Unlock()

	// Collect first to avoid mutating the bucket map during iteration.
	toDrop := make([]*node, 0, n)
	b.EachNode(func(bn *node) bool {
		if nodeIsBad(root, bn) {
			toDrop = append(toDrop, bn)
		}
		return len(toDrop) < n
	})

	for _, bn := range toDrop {
		tbl.dropNode(root, bn)
	}

	return b.Len() - tbl.k
}

func (tbl *table) dropNode(root int160.T, n *node) {
	ap := n.Addr.AddrPort()
	if _, ok := tbl.addrs[ap][n.Id]; !ok {
		panic("missing id for addr")
	}
	delete(tbl.addrs[ap], n.Id)
	if len(tbl.addrs[ap]) == 0 {
		delete(tbl.addrs, ap)
	}
	b := tbl.bucketForID(root, n.Id)

	b.Remove(n)
}

func (tbl *table) bucketForID(root int160.T, id int160.T) *bucket {
	return &tbl.buckets[tbl.bucketIndex(root, id)]
}

func (tbl *table) numNodes() (num int) {
	for i := range tbl.buckets {
		num += tbl.buckets[i].Len()
	}
	return num
}

func (tbl *table) bucketIndex(root, id int160.T) int {
	if id == root {
		panic("nobody puts the root ID in a bucket")
	}

	var a int160.T
	a.Xor(&root, &id)
	index := 160 - a.BitLen()
	return index
}

func (tbl *table) each() iter.Seq[*node] {
	return func(yield func(*node) bool) {
		for i := range tbl.buckets {
			for n := range tbl.buckets[i].NodeIter() {
				if !yield(n) {
					return
				}
			}
		}
	}
}

func (tbl *table) forNodes(f func(*node) bool) bool {
	for i := range tbl.buckets {
		if !tbl.buckets[i].EachNode(f) {
			return false
		}
	}
	return true
}

func (tbl *table) getNode(root int160.T, addr Addr, id int160.T) *node {
	if id == root {
		return nil
	}
	return tbl.buckets[tbl.bucketIndex(root, id)].GetNode(addr, id)
}

func (tbl *table) closestNodes(root int160.T, k int, target int160.T, filter func(*node) bool) (ret []*node) {
	tbl.m.Lock()
	defer tbl.m.Unlock()

	for bi := func() int {
		if target == root {
			return len(tbl.buckets) - 1
		} else {
			return tbl.bucketIndex(root, target)
		}
	}(); bi >= 0 && len(ret) < k; bi-- {
		for n := range tbl.buckets[bi].NodeIter() {
			if filter(n) {
				ret = append(ret, n)
			}
		}
	}

	// sort by distance to target
	slices.SortStableFunc(ret, nodeclosest(target))
	// keep only the closest k.
	return ret[:min(len(ret), k)]
}

func (tbl *table) addNode(root int160.T, n *node) error {
	if n.Id == root {
		return errors.New("is root id")
	}
	b := tbl.bucketForID(root, n.Id)
	if b.Len() >= tbl.k {
		return errors.New("bucket is full")
	}
	if b.GetNode(n.Addr, n.Id) != nil {
		return errors.New("already present")
	}

	defer b.AddNode(n, tbl.k)
	tbl.m.Lock()
	defer tbl.m.Unlock()

	if tbl.addrs == nil {
		tbl.addrs = make(map[netip.AddrPort]map[int160.T]struct{}, 160*tbl.k)
	}
	ap := n.Addr.AddrPort()
	if tbl.addrs[ap] == nil {
		tbl.addrs[ap] = make(map[int160.T]struct{}, 1)
	}

	tbl.addrs[ap][n.Id] = struct{}{}
	return nil
}

func (tbl *table) isGood(root int160.T, n *node) bool {
	if nodeIsBad(root, n) {
		return false
	}

	return time.Since(n.lastGotResponse) < 15*time.Minute ||
		!n.lastGotResponse.IsZero() && time.Since(n.lastGotQuery) < 15*time.Minute
}

func (tbl *table) isQuestionable(root int160.T, n *node) bool {
	return !tbl.isGood(root, n) && !nodeIsBad(root, n)
}
