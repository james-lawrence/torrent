package torrent_test

import (
	"context"
	"crypto/md5"
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/stretchr/testify/require"
)

func tcpOnlyClient(t *testing.T, dir string) *torrent.Client {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)

	return testx.Must(torrent.NewSocketsBind(sockets.New(l, &net.Dialer{})).Bind(torrent.NewClient(
		torrent.NewDefaultClientConfig(
			torrent.NewMetadataCache(dir),
			storage.NewFile(dir),
			torrent.ClientConfigCacheDirectory(t.TempDir()),
			torrent.ClientConfigSeed(true),
		),
	)))(t)
}

// clients without a packet socket (no dht server of their own) must still be able to transfer,
// the identity peers know each other by comes from a detached dht server the client starts with.
func TestTCPOnlyClientsTransfer(t *testing.T) {
	const torrentlen = bytesx.MiB

	ctx, done := testx.Context(t)
	defer done()

	sdir := t.TempDir()
	info, expected, err := torrenttest.Random(sdir, torrentlen)
	require.NoError(t, err)

	smd, err := torrent.NewFromInfo(info, torrent.OptionStorage(storage.NewFile(sdir)))
	require.NoError(t, err)

	seeder := tcpOnlyClient(t, sdir)
	defer seeder.Close()

	stor, _, err := seeder.Start(smd)
	require.NoError(t, err)
	require.NoError(t, torrent.Verify(ctx, stor))

	lmd, err := torrent.NewFromInfo(info)
	require.NoError(t, err)

	leecher := tcpOnlyClient(t, t.TempDir())
	defer leecher.Close()

	dl, _, err := leecher.Start(lmd, torrent.TuneClientPeer(seeder), torrent.TuneNewConns)
	require.NoError(t, err)

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// DownloadInto does not return when the peers never connect, so bound it
	// here - otherwise a failure hangs the test binary instead of reporting.
	type downloaded struct {
		n      int64
		digest []byte
		err    error
	}
	result := make(chan downloaded, 1)
	go func() {
		actual := md5.New()
		n, err := torrent.DownloadInto(dctx, actual, dl)
		result <- downloaded{n: n, digest: actual.Sum(nil), err: err}
	}()

	select {
	case res := <-result:
		require.NoError(t, res.err)
		require.EqualValues(t, torrentlen, res.n)
		require.Equal(t, expected.Sum(nil), res.digest)
	case <-dctx.Done():
		require.Failf(t, "leecher never completed the transfer", "completed %d of %d bytes", dl.Stats().Downloaded, torrentlen)
	}
}
