package torrent_test

import (
	"context"
	"crypto/md5"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/pubsub"
	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/james-lawrence/torrent/torrenttestx"
	"github.com/stretchr/testify/require"
)

// TestLoopbackClientPeerTransfer covers the plainest possible two-client
// transfer over loopback: a seeder and a leecher both built by
// torrenttestx.QuickClient, wired together with TuneClientPeer. This is the
// shape downstream consumers use in their own tests, and it is otherwise
// unexercised here.
func TestLoopbackClientPeerTransfer(t *testing.T) {
	const torrentlen = bytesx.MiB

	ctx, done := testx.Context(t)
	defer done()

	sdir := t.TempDir()
	info, expected, err := torrenttest.Random(sdir, torrentlen)
	require.NoError(t, err)

	smd, err := torrent.NewFromInfo(info, torrent.OptionStorage(storage.NewFile(sdir)))
	require.NoError(t, err)

	sclient := torrenttestx.QuickClient(t)
	defer sclient.Close()

	stor, _, err := sclient.Start(smd)
	require.NoError(t, err)
	require.NoError(t, torrent.Verify(ctx, stor))

	lmd, err := torrent.NewFromInfo(info)
	require.NoError(t, err)

	tclient := torrenttestx.QuickClient(t)
	defer tclient.Close()

	dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
	require.NoError(t, err)
	require.True(t, added)

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
		require.Failf(t, "leecher never completed the transfer", "completed %d of %d bytes", dl.BytesCompleted(), torrentlen)
	}
}

// TestLoopbackClientPeerTransferPreStarted mirrors how a long lived service
// observes a download: the torrent is started info-less from its infohash and
// subscribed to first (the shape of a status websocket handler), and only
// afterwards is the same infohash started again with the info and its peers.
// The subscription must report the transfer's progress.
func TestLoopbackClientPeerTransferPreStarted(t *testing.T) {
	const torrentlen = bytesx.MiB

	var sub pubsub.Subscription

	ctx, done := testx.Context(t)
	defer done()

	sdir := t.TempDir()
	info, _, err := torrenttest.Random(sdir, torrentlen)
	require.NoError(t, err)

	smd, err := torrent.NewFromInfo(info, torrent.OptionStorage(storage.NewFile(sdir)))
	require.NoError(t, err)

	sclient := torrenttestx.QuickClient(t)
	defer sclient.Close()

	stor, _, err := sclient.Start(smd)
	require.NoError(t, err)
	require.NoError(t, torrent.Verify(ctx, stor))

	tclient := torrenttestx.QuickClient(t)
	defer tclient.Close()

	// the handler's view: infohash only, no info, subscribed for updates.
	observed, err := torrent.New(metainfo.Hash(smd.ID.AsByteArray()), torrent.OptionStorage(storage.NewFile(t.TempDir())))
	require.NoError(t, err)

	watched, _, err := tclient.Start(observed, torrent.TuneSubscribe(&sub))
	require.NoError(t, err)
	defer sub.Close()

	// the request that actually kicks the download off.
	lmd, err := torrent.NewFromInfo(info)
	require.NoError(t, err)

	dl, _, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
	require.NoError(t, err)
	require.NoError(t, torrent.Verify(ctx, dl))

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for watched.BytesCompleted() < torrentlen {
		select {
		case <-sub.Values:
		case <-dctx.Done():
			require.Failf(t, "subscription never reported a completed download", "completed %d of %d bytes", watched.BytesCompleted(), torrentlen)
		}
	}
}
