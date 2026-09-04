package torrent

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrioritizedPeers(t *testing.T) {
	pp := newPeerPool(3, func(p Peer) peerPriority {
		return bep40PriorityIgnoreError(p.AddrPort, errorsx.Must(netip.ParseAddrPort("0.0.0.0:0")))
	})
	_, ok := pp.DeleteMin()
	assert.False(t, ok)
	_, ok = pp.PopMax()
	assert.False(t, ok)

	ps := []Peer{
		NewPeer(int160.Zero(), errorsx.Must(netip.ParseAddrPort("1.2.3.4:0"))),
		NewPeer(int160.Zero(), errorsx.Must(netip.ParseAddrPort("[1::2]:0"))),
		NewPeer(int160.Zero(), errorsx.Must(netip.ParseAddrPort("0.0.0.0:0"))),
		NewPeer(int160.Zero(), errorsx.Must(netip.ParseAddrPort("0.0.0.0:0")), PeerOptionTrusted(true)),
	}
	for i, p := range ps {
		// log.Printf("peer %d priority: %08x trusted: %t - %v\n", i, pp.getPrio(p), p.Trusted, p.addr())
		require.False(t, pp.Add(p))
		require.True(t, pp.Add(p))
		require.Equal(t, i+1, pp.Len())
	}
	pop := func(expected *Peer) {
		if expected == nil {
			_, ok := pp.PopMax()
			assert.False(t, ok)
		} else {
			actual, ok := pp.PopMax()
			assert.True(t, ok)
			assert.Equal(t, *expected, actual.p)
		}
	}
	min := func(expected *Peer) {
		i, ok := pp.DeleteMin()
		if expected == nil {
			assert.False(t, ok)
		} else {
			assert.True(t, ok)
			assert.Equal(t, *expected, i.p)
		}
	}
	pop(&ps[3])
	pop(&ps[1])
	min(&ps[2])
	pop(&ps[0])
	min(nil)
	pop(nil)
}

// TestPeerPoolPopMaxReservesAtomically proves a peer is considered active
// the instant PopMax removes it, not only once a later, separate Loaned
// call lands. Without that, a peer added twice in quick succession (e.g.
// TuneClientPeer adding the same AddrPort once per listener, since Autosocket
// binds uTP and TCP to the same port number) can be popped and dialed twice
// concurrently: the second Add races the window between the first pop and
// its eventual Loaned call, finds the peer in none of attempted/available/
// loaned, and is wrongly treated as a brand new peer.
func TestPeerPoolPopMaxReservesAtomically(t *testing.T) {
	pp := newPeerPool(8, func(Peer) peerPriority { return 0 })
	p := NewPeer(int160.Zero(), errorsx.Must(netip.ParseAddrPort("1.2.3.4:6001")), PeerOptionTrusted(true))

	require.False(t, pp.Add(p), "sanity: first add is a genuinely new insert")

	popped, ok := pp.PopMax()
	require.True(t, ok)
	require.Equal(t, p.AddrPort, popped.p.AddrPort)

	require.True(t, pp.Add(p), "a peer must already be considered active immediately after being popped, before any separate Loaned call")

	_, ok = pp.PopMax()
	require.False(t, ok, "the same peer must not be poppable a second time while its first pop is still outstanding")
}
