package torrent

import (
	"net/netip"
	"sync"
	"time"

	"github.com/anacrolix/multiless"
	"github.com/google/btree"
)

// Peers are stored with their priority at insertion. Their priority may
// change if our apparent IP changes, we don't currently handle that.
type prioritizedPeer struct {
	prio peerPriority
	p    Peer
}

func priorityPeerCmp(a, b prioritizedPeer) bool {
	return multiless.New().Bool(
		a.p.Trusted, b.p.Trusted,
	).Uint32(
		a.prio, b.prio,
	).Less()
}

func newPeerPool(n int, prio func(Peer) peerPriority) peerPool {
	return peerPool{
		m:         &sync.RWMutex{},
		om:        btree.NewG(n, priorityPeerCmp),
		attempted: btree.NewG(n, priorityPeerCmp),
		loaned:    make(map[netip.AddrPort]Peer, 32),
		getPrio:   prio,
		nextswap:  time.Now().Add(time.Minute),
	}
}

type peerPool struct {
	m         *sync.RWMutex
	om        *btree.BTreeG[prioritizedPeer]
	attempted *btree.BTreeG[prioritizedPeer]
	loaned    map[netip.AddrPort]Peer
	getPrio   func(Peer) peerPriority
	nextswap  time.Time
}

func (t peerPool) prioritized(p Peer) prioritizedPeer {
	return prioritizedPeer{prio: t.getPrio(p), p: p}
}

func (t *peerPool) Each(f func(Peer)) {
	t.m.RLock()
	defer t.m.RUnlock()

	t.om.Ascend(func(item prioritizedPeer) bool {
		f(item.p)
		return true
	})

	t.attempted.Ascend(func(item prioritizedPeer) bool {
		f(item.p)
		return true
	})

	for _, p := range t.loaned {
		f(p)
	}
}

func (t *peerPool) Stats() (pending, connecting int) {
	t.m.RLock()
	defer t.m.RUnlock()
	return t.om.Len(), len(t.loaned)
}

func (t *peerPool) Len() int {
	t.m.RLock()
	defer t.m.RUnlock()
	return t.om.Len()
}

func (t *peerPool) Connecting(p Peer) bool {
	t.m.RLock()
	defer t.m.RUnlock()
	_, ok := t.loaned[p.AddrPort]
	return ok
}

// Peer is returned to the pool
func (t *peerPool) Attempted(p Peer, err error) {
	t.m.Lock()
	defer t.m.Unlock()

	delete(t.loaned, p.AddrPort)

	if err != nil {
		return
	}

	t.attempted.ReplaceOrInsert(t.prioritized(p))
}

func (t *peerPool) Loaned(p Peer) {
	t.m.Lock()
	defer t.m.Unlock()

	t.loaned[p.AddrPort] = p
}

// Returns true if a peer is replaced.
func (t *peerPool) Add(p Peer) bool {
	t.m.Lock()
	defer t.m.Unlock()
	_, replaced := t.om.ReplaceOrInsert(t.prioritized(p))
	return replaced
}

func (t *peerPool) DeleteMin() (ret prioritizedPeer, ok bool) {
	t.m.Lock()
	defer t.m.Unlock()
	return t.om.DeleteMin()
}

func (t *peerPool) PopMax() (p prioritizedPeer, ok bool) {
	t.m.Lock()
	defer t.m.Unlock()

	if max, present := t.om.DeleteMax(); present {
		return max, present
	}

	if ts := time.Now(); t.nextswap.After(ts) {
		return p, false
	}

	t.om = t.attempted.Clone()
	t.attempted.Clear(true)

	return t.om.DeleteMax()
}
