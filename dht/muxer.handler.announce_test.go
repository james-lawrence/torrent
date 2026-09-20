package dht

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	peer_store "github.com/james-lawrence/torrent/dht/peer-store"
	"github.com/stretchr/testify/require"
)

func TestHandlerAnnounce(t *testing.T) {
	t.Run("stores the peer under the announced port", func(t *testing.T) {
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

		res := querier.announcePeer(t.Context(), receiverAddr, infohash, 6881, receiver.createToken(querierAddr), false)
		require.NoError(t, res.Err)

		peers := receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray()))
		require.Len(t, peers, 1)
		require.Equal(t, netip.MustParseAddr("127.0.0.1"), peers[0].Addr().Unmap())
		require.EqualValues(t, 6881, peers[0].Port())
	})

	t.Run("stores the source port of the announce when the port is implied", func(t *testing.T) {
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

		// the explicit port is ignored when the port is implied.
		res := querier.announcePeer(t.Context(), receiverAddr, infohash, 1234, receiver.createToken(querierAddr), true)
		require.NoError(t, res.Err)

		peers := receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray()))
		require.Len(t, peers, 1)
		require.EqualValues(t, qconn.LocalAddr().(*net.UDPAddr).Port, peers[0].Port())
	})

	t.Run("ignores an announce whose token was issued to another address", func(t *testing.T) {
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
		elsewhere := NewAddr(netip.MustParseAddrPort("192.0.2.1:6881"))

		// an announce with an invalid token is never answered, so the query only ends with its context.
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		res := querier.announcePeer(ctx, receiverAddr, infohash, 6881, receiver.createToken(elsewhere), false)
		require.Error(t, res.Err)
		require.Empty(t, receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray())))
	})

	t.Run("does not store a peer that announced no usable port", func(t *testing.T) {
		for name, port := range map[string]*uint16{
			"no port":        nil,
			"a port of zero": new(uint16),
		} {
			t.Run(name, func(t *testing.T) {
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

				// announcePeer refuses to build this request, so it is built by hand.
				qi, err := NewMessageRequest("announce_peer", &krpc.MsgArgs{
					ID:       querier.ID(receiverAddr.AddrPort()).AsByteArray(),
					InfoHash: infohash.AsByteArray(),
					Port:     port,
					Token:    receiver.createToken(querierAddr),
				})
				require.NoError(t, err)

				// the token is valid so the announce is still answered, it just isn't stored.
				res := querier.Query(t.Context(), receiverAddr, qi)
				require.NoError(t, res.Err)
				require.Empty(t, receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray())))
			})
		}
	})

	t.Run("keeps one entry when the same address and port announces again", func(t *testing.T) {
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

		for range 2 {
			res := querier.announcePeer(t.Context(), receiverAddr, infohash, 6881, receiver.createToken(querierAddr), false)
			require.NoError(t, res.Err)
		}

		require.Len(t, receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray())), 1)
	})

	t.Run("keeps each port an address announces", func(t *testing.T) {
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

		for _, port := range []uint16{6881, 6882} {
			res := querier.announcePeer(t.Context(), receiverAddr, infohash, port, receiver.createToken(querierAddr), false)
			require.NoError(t, res.Err)
		}

		peers := receiver.peers.GetPeers(peer_store.InfoHash(infohash.AsByteArray()))
		require.Len(t, peers, 2)
	})

	t.Run("reports the announced port to the announce hooks, not the port the packet came from", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		announced := make(chan netip.AddrPort, 1)
		receiver, err := NewServer(
			32,
			OptionOnAnnouncePeer(PeerAnnounceFn(func(peerid int160.T, source netip.AddrPort, portOk bool) {
				announced <- source
			})),
		)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		receiverAddr := NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort())
		querierAddr := NewAddr(qconn.LocalAddr().(*net.UDPAddr).AddrPort())

		// the announce is sent from the querier's socket, which is not port 6881.
		res := querier.announcePeer(t.Context(), receiverAddr, int160.Random(), 6881, receiver.createToken(querierAddr), false)
		require.NoError(t, res.Err)

		select {
		case peer := <-announced:
			require.EqualValues(t, 6881, peer.Port(), "the hook must be told the port that was announced, otherwise the peer is recorded at an address nothing is listening on")
		case <-time.After(time.Second):
			t.Fatal("the announce hook was not called")
		}
	})

	t.Run("tells the announce hooks when the announce supplied no usable port", func(t *testing.T) {
		for name, port := range map[string]*uint16{
			"no port":        nil,
			"a port of zero": new(uint16),
		} {
			t.Run(name, func(t *testing.T) {
				rconn := mustListen("127.0.0.1:0")
				qconn := mustListen("127.0.0.1:0")

				type announce struct {
					peer   netip.AddrPort
					portOk bool
				}

				announced := make(chan announce, 1)
				receiver, err := NewServer(
					32,
					OptionOnAnnouncePeer(PeerAnnounceFn(func(peerid int160.T, peer netip.AddrPort, portOk bool) {
						announced <- announce{peer: peer, portOk: portOk}
					})),
				)
				require.NoError(t, err)
				backgroundServe(t, receiver, rconn)
				defer receiver.Close()

				querier, err := NewServer(32)
				require.NoError(t, err)
				backgroundServe(t, querier, qconn)
				defer querier.Close()

				receiverAddr := NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort())
				querierAddr := NewAddr(qconn.LocalAddr().(*net.UDPAddr).AddrPort())

				// announcePeer refuses to build this request, so it is built by hand.
				qi, err := NewMessageRequest("announce_peer", &krpc.MsgArgs{
					ID:       querier.ID(receiverAddr.AddrPort()).AsByteArray(),
					InfoHash: int160.Random().AsByteArray(),
					Port:     port,
					Token:    receiver.createToken(querierAddr),
				})
				require.NoError(t, err)

				res := querier.Query(t.Context(), receiverAddr, qi)
				require.NoError(t, res.Err)

				select {
				case got := <-announced:
					require.False(t, got.portOk)
					require.Zero(t, got.peer.Port())
				case <-time.After(time.Second):
					t.Fatal("the announce hook was not called")
				}
			})
		}
	})

	t.Run("tells the announce hooks the announce supplied a port", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		qconn := mustListen("127.0.0.1:0")

		announced := make(chan bool, 2)
		receiver, err := NewServer(
			32,
			OptionOnAnnouncePeer(PeerAnnounceFn(func(peerid int160.T, source netip.AddrPort, portOk bool) {
				announced <- portOk
			})),
		)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		querier, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, querier, qconn)
		defer querier.Close()

		receiverAddr := NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort())
		querierAddr := NewAddr(qconn.LocalAddr().(*net.UDPAddr).AddrPort())

		res := querier.announcePeer(t.Context(), receiverAddr, int160.Random(), 6881, receiver.createToken(querierAddr), false)
		require.NoError(t, res.Err)

		select {
		case portOk := <-announced:
			require.True(t, portOk)
		case <-time.After(time.Second):
			t.Fatal("the announce hook was not called")
		}
	})
}
