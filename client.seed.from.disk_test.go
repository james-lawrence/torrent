package torrent_test

import (
	"context"
	"crypto/md5"
	"sync/atomic"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/james-lawrence/torrent/torrenttestx"
	"github.com/stretchr/testify/require"
)

// TestClientSeedFromDisk covers a seeder whose torrent only exists on disk. the torrent is
// loaded into memory when a leecher connects, the uploaded bytes are recorded when the
// connection closes, and the torrent is unloaded once it has been idle.
func TestClientSeedFromDisk(t *testing.T) {
	const (
		torrentlen  = bytesx.MiB
		idletimeout = 500 * time.Millisecond
	)

	t.Run("loads from disk, records uploads, and unloads when idle", func(t *testing.T) {
		var uploaded atomic.Int64

		ctx, done := testx.Context(t)
		defer done()

		sdir := t.TempDir()
		info, expected, err := torrenttest.Random(sdir, torrentlen)
		require.NoError(t, err)

		smd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		// the torrent is on disk, but never started.
		mdstore := torrent.NewMetadataCache(t.TempDir())
		require.NoError(t, mdstore.Write(smd))

		sclient := torrenttestx.Client(
			t,
			autobind.NewLoopback(autobind.EnableDHT(torrenttestx.QuickDHT(t))),
			mdstore,
			storage.NewFile(sdir),
			torrent.ClientConfigIdleTimeout(idletimeout),
			torrent.ClientConfigConnectionClosed(func(_ int160.T, stats torrent.ConnStats, _ int) {
				uploaded.Add(stats.BytesWrittenData.Int64())
			}),
		)
		defer sclient.Close()

		resident := 0
		require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
			resident++
			return false
		})))
		require.Zero(t, resident, "seeder must not have the torrent in memory before a leecher connects")

		lmd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		tclient := torrenttestx.QuickClient(t)
		defer tclient.Close()

		dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
		require.NoError(t, err)
		require.True(t, added)

		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

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

		// the leecher is still connected, so the seeder holds on to the torrent even past the idle timeout.
		require.Never(t, func() bool {
			resident := 0
			require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			return resident == 0
		}, 2*idletimeout, idletimeout/10, "seeder unloaded the torrent while a leecher was still connected")

		require.NoError(t, tclient.Stop(lmd))

		require.Eventually(t, func() bool {
			resident := 0
			require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			return resident == 0
		}, 10*idletimeout, idletimeout/10, "seeder did not unload the idle torrent")

		require.GreaterOrEqual(t, uploaded.Load(), int64(torrentlen), "seeder did not record its uploaded bytes")
	})

	t.Run("unloads a download that makes no progress", func(t *testing.T) {
		info, _, err := torrenttest.Random(t.TempDir(), torrentlen)
		require.NoError(t, err)

		md, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		client := torrenttestx.QuickClient(t, torrent.ClientConfigIdleTimeout(idletimeout))
		defer client.Close()

		_, added, err := client.Start(md)
		require.NoError(t, err)
		require.True(t, added)

		resident := 0
		require.NoError(t, client.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
			resident++
			return false
		})))
		require.Equal(t, 1, resident)

		require.Eventually(t, func() bool {
			resident := 0
			require.NoError(t, client.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			return resident == 0
		}, 10*idletimeout, idletimeout/10, "client did not unload the stalled torrent")
	})
}
