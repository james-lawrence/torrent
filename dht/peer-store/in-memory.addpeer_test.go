package peer_store

import (
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/stretchr/testify/require"
)

func TestInMemoryAddPeer(t *testing.T) {
	ih := InfoHash{1}
	other := InfoHash{2}
	a := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 6881)
	b := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 2), 6881)
	c := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 3), 6881)
	d := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 4), 6881)

	t.Run("evicts the oldest announce once an infohash is at its cap", func(t *testing.T) {
		store := &InMemory{MaxPeers: 3}

		// the sleeps keep the announce times distinct, so the eviction order is deterministic.
		for _, p := range []krpc.NodeAddr{a, b, c, d} {
			store.AddPeer(ih, p)
			time.Sleep(time.Millisecond)
		}

		require.ElementsMatch(t, []krpc.NodeAddr{b, c, d}, store.GetPeers(ih))
	})

	t.Run("defaults to 256 peers per infohash", func(t *testing.T) {
		var store InMemory

		for i := range 300 {
			store.AddPeer(ih, krpc.NewNodeAddrFromIPPort(net.IPv4(10, 1, byte(i>>8), byte(i)), 6881))
		}

		require.Len(t, store.GetPeers(ih), 256)
		require.Equal(t, DefaultMaxPeers, len(store.GetPeers(ih)))
	})

	t.Run("caps each infohash separately", func(t *testing.T) {
		store := &InMemory{MaxPeers: 2}

		for _, p := range []krpc.NodeAddr{a, b, c} {
			store.AddPeer(ih, p)
			store.AddPeer(other, p)
		}

		require.Len(t, store.GetPeers(ih), 2)
		require.Len(t, store.GetPeers(other), 2)
	})

	t.Run("keeps the different ports of one address", func(t *testing.T) {
		var store InMemory
		first := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7001)
		second := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7002)

		store.AddPeer(ih, first)
		store.AddPeer(ih, second)

		require.ElementsMatch(t, []krpc.NodeAddr{first, second}, store.GetPeers(ih))
	})

	t.Run("limits the ports one address holds, evicting its oldest first", func(t *testing.T) {
		store := &InMemory{MaxPeersPerIP: 2}
		p1 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7001)
		p2 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7002)
		p3 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7003)

		// b is the oldest announce overall, but it is a different address so it is not evicted.
		for _, p := range []krpc.NodeAddr{b, p1, p2, p3} {
			store.AddPeer(ih, p)
			time.Sleep(time.Millisecond)
		}

		require.ElementsMatch(t, []krpc.NodeAddr{b, p2, p3}, store.GetPeers(ih))
	})

	t.Run("defaults to 8 ports per address so one address cannot fill an infohash", func(t *testing.T) {
		var store InMemory

		store.AddPeer(ih, a)
		for port := range 300 {
			store.AddPeer(ih, krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 9), uint16(8000+port)))
		}

		peers := store.GetPeers(ih)
		require.Len(t, peers, 1+DefaultMaxPeersPerIP)
		require.Contains(t, peers, a)
	})

	t.Run("announcing a known address and port again does not use another slot", func(t *testing.T) {
		store := &InMemory{MaxPeersPerIP: 2}
		p1 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7001)
		p2 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7002)
		p3 := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 7003)

		for _, p := range []krpc.NodeAddr{p1, p2, p1, p3} {
			store.AddPeer(ih, p)
			time.Sleep(time.Millisecond)
		}

		// p1 was refreshed, so p2 is the oldest of the address.
		require.ElementsMatch(t, []krpc.NodeAddr{p1, p3}, store.GetPeers(ih))
	})

	t.Run("refreshes a known address instead of evicting", func(t *testing.T) {
		store := &InMemory{MaxPeers: 2}

		store.AddPeer(ih, a)
		time.Sleep(time.Millisecond)
		store.AddPeer(ih, b)
		time.Sleep(time.Millisecond)
		// a is at the cap but already known, announcing again makes it the newest and evicts nothing.
		store.AddPeer(ih, a)
		require.ElementsMatch(t, []krpc.NodeAddr{a, b}, store.GetPeers(ih))
		time.Sleep(time.Millisecond)
		// b is now the oldest.
		store.AddPeer(ih, c)

		require.ElementsMatch(t, []krpc.NodeAddr{a, c}, store.GetPeers(ih))
	})

	t.Run("drops the expired peers of the infohash being announced", func(t *testing.T) {
		store := &InMemory{TTL: 200 * time.Millisecond}

		store.AddPeer(ih, a)
		time.Sleep(250 * time.Millisecond)
		store.AddPeer(ih, b)

		require.ElementsMatch(t, []krpc.NodeAddr{b}, store.GetPeers(ih))
		require.Len(t, store.GetAll()[ih], 1)
	})

	t.Run("releases infohashes that are never announced to again", func(t *testing.T) {
		store := &InMemory{TTL: 200 * time.Millisecond}

		store.AddPeer(ih, a)
		time.Sleep(250 * time.Millisecond)
		// announcing to any infohash sweeps the rest of the index.
		store.AddPeer(other, b)

		all := store.GetAll()
		require.NotContains(t, all, ih)
		require.Len(t, all, 1)
		require.ElementsMatch(t, []krpc.NodeAddr{b}, store.GetPeers(other))
	})
}
