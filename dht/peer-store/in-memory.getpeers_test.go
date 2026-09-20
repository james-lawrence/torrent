package peer_store

import (
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/stretchr/testify/require"
)

func TestInMemoryGetPeers(t *testing.T) {
	ih := InfoHash{1}
	a := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 1), 6881)
	b := krpc.NewNodeAddrFromIPPort(net.IPv4(10, 0, 0, 2), 6881)

	t.Run("returns nothing for an unknown infohash", func(t *testing.T) {
		var store InMemory

		require.Empty(t, store.GetPeers(ih))
	})

	t.Run("returns the announced peers", func(t *testing.T) {
		var store InMemory

		store.AddPeer(ih, a)
		store.AddPeer(ih, b)

		require.ElementsMatch(t, []krpc.NodeAddr{a, b}, store.GetPeers(ih))
	})

	t.Run("does not return expired peers, even before they are removed", func(t *testing.T) {
		store := &InMemory{TTL: 200 * time.Millisecond}

		store.AddPeer(ih, a)
		require.Len(t, store.GetPeers(ih), 1)

		// nothing announces in between, so nothing has removed the peer.
		time.Sleep(250 * time.Millisecond)

		require.Empty(t, store.GetPeers(ih))
	})
}
