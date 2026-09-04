package torrent_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/james-lawrence/torrent/torrenttestx"
	"github.com/stretchr/testify/require"
)

// TestDirectPeerWildcardBindLoopback reproduces, end-to-end, a real failure
// seen by a downstream consumer: a client bound wildcard - the shape
// autobind.Local produces, used when a caller needs to wrap the raw uTP
// socket/TCP listener itself (e.g. to inject a connection limiter) before
// binding - could not fetch torrent info from a loopback-bound seeder over a
// direct, address-only peer connection.
//
// The self-connection check in initiateConn compares
// dht.Server.ID(peer.AddrPort) against peer.ID; for a peer built from a bare
// address the real ID isn't knowable, so it's built with int160.Zero() as a
// placeholder (see NewPeerDeprecated). That collided with Server.ID's own
// Zero() fallback for any address outside a wildcard socket's registered
// bindings, so every direct peer at a loopback address was silently treated
// as "self" and never dialed - Info() then blocked until its context
// deadline. This is fixed by Server.Serve registering a binding per
// reachability scope (loopback/link-local/routed) for a wildcard bind,
// instead of collapsing to a single routed-scope "best" pick.
func TestDirectPeerWildcardBindLoopback(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()

	seederDHT, err := dht.NewServer(32, dht.OptionBootstrapNodesNone)
	require.NoError(t, err)
	seeder := torrenttestx.QuickClientWithDHT(t, seederDHT)
	defer seeder.Close()

	seedDir := t.TempDir()
	info, _, err := torrenttest.Random(seedDir, 32*bytesx.KiB)
	require.NoError(t, err)

	encoded, err := bencode.Marshal(info)
	require.NoError(t, err)
	hash := metainfo.NewHashFromBytes(encoded)

	seedermd, err := torrent.NewFromInfo(info, torrent.OptionStorage(storage.NewFile(seedDir)))
	require.NoError(t, err)
	_, added, err := seeder.Start(seedermd, torrent.TuneAnnounceUntilComplete, torrent.TuneNewConns)
	require.NoError(t, err)
	require.True(t, added)

	var seederAddr netip.AddrPort
	for _, a := range seeder.ListenAddrs() {
		if a.Network() != "tcp" {
			continue
		}
		ap, err := netx.AddrPort(a)
		require.NoError(t, err)
		seederAddr = ap
		break
	}
	require.True(t, seederAddr.IsValid(), "seeder should have a tcp listen address")

	// leecher: wildcard-bound, exactly mirroring how autobind.Local is meant
	// to be used - raw components, wrapped by the caller (here, unwrapped -
	// a connection limiter is a downstream concern), bound via
	// torrent.NewSocketsBind.
	u, l, err := autobind.Local("udp", 0)
	require.NoError(t, err)

	leechDHT, err := dht.NewServer(32, dht.OptionBootstrapNodesNone)
	require.NoError(t, err)

	leecher, err := torrent.NewSocketsBind(
		sockets.New(u, u),
		sockets.New(l, &net.Dialer{}),
	).Options(torrent.BinderOptionDHT(leechDHT)).Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer leecher.Close()

	leechmd, err := torrent.New(hash, torrent.OptionStorage(storage.NewFile(t.TempDir())))
	require.NoError(t, err)

	peers := []torrent.Peer{
		torrent.NewPeerDeprecated(int160.Zero(), net.IP(seederAddr.Addr().AsSlice()), seederAddr.Port(), torrent.PeerOptionTrusted(true)),
	}

	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	fetched, err := leecher.Info(infoCtx, leechmd, torrent.TuneAnnounceUntilComplete, torrent.TuneNewConns, torrent.TunePeers(peers...))
	require.NoError(t, err)
	require.NotNil(t, fetched)
}
