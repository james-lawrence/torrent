package peer_store

import (
	"net/netip"
	"sync"
	"time"

	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/langx"
)

const (
	// DefaultMaxPeers is the most peers remembered for a single infohash.
	DefaultMaxPeers = 256
	// DefaultMaxPeersPerIP is the most ports a single address is remembered on for an infohash.
	DefaultMaxPeersPerIP = 8
	// DefaultTTL is how long a peer is remembered after it last announced, peers are expected
	// to announce again every 30 minutes.
	DefaultTTL = time.Hour
)

// InMemory is a peer store bounded by a cap per infohash and by expiry. the zero value is ready to use.
type InMemory struct {
	// MaxPeers is the most peers remembered per infohash, the oldest announce is evicted first.
	// defaults to DefaultMaxPeers.
	MaxPeers uint
	// MaxPeersPerIP is the most ports a single address is remembered on per infohash, the oldest
	// announce from that address is evicted first. it keeps one address, which can announce any port,
	// from filling the infohash. defaults to DefaultMaxPeersPerIP.
	MaxPeersPerIP uint
	// TTL is how long a peer is remembered after it last announced. defaults to DefaultTTL.
	TTL time.Duration

	mu    sync.RWMutex
	index map[InfoHash]indexValue
	// when the whole index was last checked for expired peers.
	swept time.Time
}

func (me *InMemory) maxPeers() uint {
	return langx.FirstNonZero(me.MaxPeers, DefaultMaxPeers)
}

func (me *InMemory) maxPeersPerIP() uint {
	return langx.FirstNonZero(me.MaxPeersPerIP, DefaultMaxPeersPerIP)
}

func (me *InMemory) ttl() time.Duration {
	if me.TTL > 0 {
		return me.TTL
	}

	return DefaultTTL
}

// prune removes the peers that have not announced within the ttl.
func prune(nodes indexValue, now time.Time, ttl time.Duration) {
	for k, v := range nodes {
		if now.Sub(v.Time) >= ttl {
			delete(nodes, k)
		}
	}
}

// evict removes the oldest peer that match accepts.
func evict(nodes indexValue, match func(netip.AddrPort) bool) {
	var (
		oldest netip.AddrPort
		found  bool
	)

	for k, v := range nodes {
		if !match(k) {
			continue
		}

		if !found || v.Time.Before(nodes[oldest].Time) {
			oldest, found = k, true
		}
	}

	if found {
		delete(nodes, oldest)
	}
}

// peers are keyed by address and port, several clients can share an address.
type indexValue = map[netip.AddrPort]NodeAndTime

func (me *InMemory) GetPeers(ih InfoHash) (ret []krpc.NodeAddr) {
	me.mu.RLock()
	defer me.mu.RUnlock()

	now, ttl := time.Now(), me.ttl()
	for _, v := range me.index[ih] {
		// expired peers are only removed when announcing, they must not be returned in the meantime.
		if now.Sub(v.Time) >= ttl {
			continue
		}

		ret = append(ret, v.NodeAddr)
	}
	return
}

func (me *InMemory) AddPeer(ih InfoHash, na krpc.NodeAddr) {
	key := netip.AddrPortFrom(na.Addr().Unmap(), na.Port())
	now, ttl := time.Now(), me.ttl()
	me.mu.Lock()
	defer me.mu.Unlock()
	if me.index == nil {
		me.index = make(map[InfoHash]indexValue)
	}

	// infohashes that are never announced to or queried again would otherwise be kept forever,
	// so periodically check them all. this is what bounds the number of infohashes.
	if now.Sub(me.swept) >= ttl/4 {
		for ih, nodes := range me.index {
			prune(nodes, now, ttl)
			if len(nodes) == 0 {
				delete(me.index, ih)
			}
		}
		me.swept = now
	}

	nodes := me.index[ih]
	if nodes == nil {
		nodes = make(indexValue)
		me.index[ih] = nodes
	}

	prune(nodes, now, ttl)

	// an address and port that is already known is refreshed below, it does not need room made for it.
	if _, known := nodes[key]; !known {
		sameip := func(k netip.AddrPort) bool { return k.Addr() == key.Addr() }

		// one address may announce any port, limit how many of the slots it can hold.
		for {
			var occupied uint
			for k := range nodes {
				if sameip(k) {
					occupied++
				}
			}

			if occupied < me.maxPeersPerIP() {
				break
			}

			evict(nodes, sameip)
		}

		for uint(len(nodes)) >= me.maxPeers() {
			evict(nodes, func(netip.AddrPort) bool { return true })
		}
	}

	nodes[key] = NodeAndTime{na, now}
}

type NodeAndTime struct {
	krpc.NodeAddr
	time.Time
}

func (me *InMemory) GetAll() (ret map[InfoHash][]NodeAndTime) {
	me.mu.RLock()
	defer me.mu.RUnlock()
	ret = make(map[InfoHash][]NodeAndTime, len(me.index))
	for ih, nodes := range me.index {
		for _, v := range nodes {
			ret[ih] = append(ret[ih], v)
		}
	}
	return
}
