package dht

import (
	"iter"
	"sync"
	"time"

	"github.com/anacrolix/chansync"

	"github.com/james-lawrence/torrent/dht/int160"
)

type bucket struct {
	_m sync.RWMutex
	// Per the "Routing Table" section of BEP 5.
	changed     chansync.BroadcastCond
	lastChanged time.Time
	nodes       map[*node]struct{}
}

func (b *bucket) Len() int {
	b._m.RLock()
	defer b._m.RUnlock()
	return len(b.nodes)
}

func (b *bucket) NodeIter() iter.Seq[*node] {
	return func(yield func(*node) bool) {
		b._m.RLock()
		snapshot := make([]*node, 0, len(b.nodes))
		for n := range b.nodes {
			snapshot = append(snapshot, n)
		}
		b._m.RUnlock()

		for _, n := range snapshot {
			if !yield(n) {
				return
			}
		}
	}
}

// Returns true if f returns true for all nodes. Iteration stops if f returns false.
func (b *bucket) EachNode(f func(*node) bool) bool {
	next, stop := iter.Pull(b.NodeIter())
	defer stop()
	for c := true; c; {
		b._m.RLock()
		v, ok := next()
		b._m.RUnlock()
		if !ok {
			return false
		}
		if !f(v) {
			return false
		}
	}

	return true
}

func (b *bucket) AddNode(n *node, k int) {
	b._m.Lock()
	defer b._m.Unlock()
	if _, ok := b.nodes[n]; ok {
		return
	}
	if b.nodes == nil {
		b.nodes = make(map[*node]struct{}, k)
	}
	b.nodes[n] = struct{}{}
	b.lastChanged = time.Now()
	b.changed.Broadcast()
}

func (b *bucket) GetNode(addr Addr, id int160.T) *node {
	b._m.RLock()
	defer b._m.RUnlock()
	for n := range b.nodes {
		if n.hasAddrAndID(addr, id) {
			return n
		}
	}
	return nil
}


func (b *bucket) Remove(n *node) {
	b._m.Lock()
	defer b._m.Unlock()
	delete(b.nodes, n)
}
