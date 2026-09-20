package torrent

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/require"
)

func TestClientOnDHTAnnouncePeer(t *testing.T) {
	t.Run("adds the peer at the port it announced", func(t *testing.T) {
		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
		)
		require.NoError(t, err)

		tor, err := cl.torrents.Insert(md, cl.newTorrent, tuneMerge(md))
		require.NoError(t, err)

		announced := netip.MustParseAddrPort("127.0.0.1:6881")
		cl.onDHTAnnouncePeer(md.ID, announced, true)

		var peers []netip.AddrPort
		tor.peers.Each(func(p Peer) { peers = append(peers, p.AddrPort) })
		require.Equal(t, []netip.AddrPort{announced}, peers)
	})

	t.Run("ignores a peer that did not announce a usable port", func(t *testing.T) {
		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
		)
		require.NoError(t, err)

		tor, err := cl.torrents.Insert(md, cl.newTorrent, tuneMerge(md))
		require.NoError(t, err)

		cl.onDHTAnnouncePeer(md.ID, netip.MustParseAddrPort("127.0.0.1:0"), false)

		var peers []netip.AddrPort
		tor.peers.Each(func(p Peer) { peers = append(peers, p.AddrPort) })
		require.Empty(t, peers)
	})
}
