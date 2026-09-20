package dht

import (
	"net"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	peer_store "github.com/james-lawrence/torrent/dht/peer-store"
	"github.com/stretchr/testify/require"
)

func TestHandlerPeers(t *testing.T) {
	t.Run("a default server issues a token in its get_peers reply", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		receiver, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		res := querier.GetPeers(t.Context(), NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort()), int160.Random(), false)
		require.NoError(t, res.Err)
		require.NotNil(t, res.Reply.R)
		require.NotNil(t, res.Reply.R.Token, "without a token no node can announce to this server, and announce traversals end with nothing to announce to")
	})

	t.Run("returns the stored peers of the infohash and no nodes", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		receiver, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		infohash := int160.Random()
		stored := krpc.NewNodeAddrFromIPPort(net.IPv4(192, 0, 2, 7), 6881)
		receiver.peers.AddPeer(peer_store.InfoHash(infohash.AsByteArray()), stored)

		res := querier.GetPeers(t.Context(), NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort()), infohash, false)
		require.NoError(t, res.Err)
		require.NotNil(t, res.Reply.R)

		require.Len(t, res.Reply.R.Values, 1)
		require.Equal(t, netip.AddrPortFrom(stored.Addr().Unmap(), stored.Port()), netip.AddrPortFrom(res.Reply.R.Values[0].Addr().Unmap(), res.Reply.R.Values[0].Port()))
		require.Empty(t, res.Reply.R.Nodes, "nodes are only returned when there are no peers")
	})

	t.Run("does not return the peers of another infohash", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		receiver, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		receiver.peers.AddPeer(peer_store.InfoHash(int160.Random().AsByteArray()), krpc.NewNodeAddrFromIPPort(net.IPv4(192, 0, 2, 7), 6881))

		res := querier.GetPeers(t.Context(), NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort()), int160.Random(), false)
		require.NoError(t, res.Err)
		require.NotNil(t, res.Reply.R)
		require.Empty(t, res.Reply.R.Values)
	})

	t.Run("returns a peer that announced to it", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		receiver, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		infohash := int160.Random()
		receiverAddr := NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort())
		querierAddr := NewAddr(qconn.LocalAddr().(*net.UDPAddr).AddrPort())

		announced := querier.announcePeer(t.Context(), receiverAddr, infohash, 6881, receiver.createToken(querierAddr), false)
		require.NoError(t, announced.Err)

		res := querier.GetPeers(t.Context(), receiverAddr, infohash, false)
		require.NoError(t, res.Err)
		require.NotNil(t, res.Reply.R)

		require.Len(t, res.Reply.R.Values, 1)
		require.EqualValues(t, 6881, res.Reply.R.Values[0].Port())
		require.Equal(t, querierAddr.IP().To4().String(), res.Reply.R.Values[0].IP().To4().String())
	})
}
