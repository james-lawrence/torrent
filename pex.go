package torrent

import (
	"net/netip"
	"sync"
	"time"

	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/dht/krpc"
)

func newPex() *pex {
	return &pex{
		m:     &sync.RWMutex{},
		conns: make(map[*connection]time.Time, 25),
	}
}

// peer exchange - http://bittorrent.org/beps/bep_0011.html
type pex struct {
	m     *sync.RWMutex
	conns map[*connection]time.Time
}

func (t *pex) snapshot(c0 *connection) *pp.PexMsg {
	t.m.RLock()
	defer t.m.RUnlock()

	if len(t.conns) == 0 {
		return nil
	}

	tx := &pp.PexMsg{}

	n := 0
	for c := range t.conns {
		if c == c0 {
			continue
		}

		if n > 25 {
			break
		}

		// remoteAddr is only trustworthy as a listening address for
		// connections we dialed - we dialed that exact address. For an
		// incoming connection, remoteAddr is the peer's ephemeral outgoing
		// source port for this socket, not their listening port; the real
		// listening port can only come from what they told us in their
		// extension handshake. A peer that never announced one must be
		// omitted, not reported with a wrong address.
		addr := c.remoteAddr
		if !c.outgoing {
			if c.PeerListenPort == 0 {
				continue
			}
			addr = netip.AddrPortFrom(addr.Addr(), c.PeerListenPort)
		}

		f := c.pexPeerFlags()
		if c.ipv6() {
			tx.Added6 = append(tx.Added6, krpc.NewNodeAddrFromAddrPort(addr))
			tx.Added6Flags = append(tx.Added6Flags, f)
		} else {
			tx.Added = append(tx.Added, krpc.NewNodeAddrFromAddrPort(addr))
			tx.AddedFlags = append(tx.AddedFlags, f)
		}

		n++
	}

	return tx
}

func (t *pex) added(c *connection) {
	t.m.Lock()
	defer t.m.Unlock()
	t.conns[c] = time.Now()
}

func (t *pex) dropped(c *connection) {
	t.m.Lock()
	defer t.m.Unlock()
	delete(t.conns, c)
}
