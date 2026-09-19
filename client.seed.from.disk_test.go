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
	"golang.org/x/time/rate"
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

	t.Run("reloads after unload and aggregates uploaded and downloaded bytes across loads", func(t *testing.T) {
		const rounds = 2

		var (
			uploaded   atomic.Int64
			downloaded atomic.Int64
		)

		ctx, done := testx.Context(t)
		defer done()

		sdir := t.TempDir()
		info, expected, err := torrenttest.Random(sdir, torrentlen)
		require.NoError(t, err)

		smd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

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

		// each round is a fresh leecher, the seeder has to load the torrent from disk every time.
		for round := range rounds {
			resident := 0
			require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			require.Zero(t, resident, "round %d: seeder must not have the torrent in memory before the leecher connects", round)

			lmd, err := torrent.NewFromInfo(info)
			require.NoError(t, err)

			tclient := torrenttestx.QuickClient(
				t,
				torrent.ClientConfigConnectionClosed(func(_ int160.T, stats torrent.ConnStats, _ int) {
					downloaded.Add(stats.BytesReadData.Int64())
				}),
			)

			dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
			require.NoError(t, err)
			require.True(t, added)

			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			actual := md5.New()
			n, err := torrent.DownloadInto(dctx, actual, dl)
			cancel()
			require.NoError(t, err, "round %d: download failed", round)
			require.EqualValues(t, torrentlen, n)
			require.Equal(t, expected.Sum(nil), actual.Sum(nil), "round %d: digest mismatch", round)

			resident = 0
			require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			require.Equal(t, 1, resident, "round %d: seeder must have loaded the torrent from disk", round)

			require.NoError(t, tclient.Stop(lmd))

			require.Eventually(t, func() bool {
				resident := 0
				require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
					resident++
					return false
				})))
				return resident == 0
			}, 10*idletimeout, idletimeout/10, "round %d: seeder did not unload the idle torrent", round)

			tclient.Close()
		}

		// both sides record their side of every connection when it closes, so once
		// everything settles the seeder's uploads must equal what the leechers received.
		require.Eventually(t, func() bool {
			return uploaded.Load() >= rounds*int64(torrentlen) && uploaded.Load() == downloaded.Load()
		}, 10*idletimeout, idletimeout/10, "uploaded %d and downloaded %d must agree and cover %d rounds of %d bytes", uploaded.Load(), downloaded.Load(), rounds, int64(torrentlen))
	})

	t.Run("serves simultaneous leechers from a single load", func(t *testing.T) {
		const leechers = 5

		var (
			uploaded   atomic.Int64
			downloaded atomic.Int64
		)

		ctx, done := testx.Context(t)
		defer done()

		sdir := t.TempDir()
		info, expected, err := torrenttest.Random(sdir, torrentlen)
		require.NoError(t, err)

		smd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		mdstore := torrent.NewMetadataCache(t.TempDir())
		require.NoError(t, mdstore.Write(smd))

		sclient := torrenttestx.Client(
			t,
			autobind.NewLoopback(autobind.EnableDHT(torrenttestx.QuickDHT(t))),
			mdstore,
			storage.NewFile(sdir),
			torrent.ClientConfigIdleTimeout(idletimeout),
			// all the leechers dial at once, well past the default accept burst (NumCPU).
			torrent.ClientConfigAcceptLimit(rate.NewLimiter(rate.Inf, 0)),
			// a leecher can vanish without its close reaching the seeder (a connection dialed while it shuts down),
			// the seeder drops that connection after two keepalive intervals. the default of 10s is longer than the
			// unload window below, so shorten it. the leechers match, a peer with a longer interval is dropped while quiet.
			torrent.ClientConfigKeepAlive(time.Second),
			torrent.ClientConfigConnectionClosed(func(_ int160.T, stats torrent.ConnStats, _ int) {
				uploaded.Add(stats.BytesWrittenData.Int64())
				downloaded.Add(stats.BytesReadData.Int64())
			}),
		)
		defer sclient.Close()

		resident := 0
		require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
			resident++
			return false
		})))
		require.Zero(t, resident, "seeder must not have the torrent in memory before the leechers connect")

		type result struct {
			n      int64
			digest []byte
			err    error
		}

		var (
			clients [leechers]*torrent.Client
			results [leechers]chan result
		)

		// start every leecher before waiting on any of them so the downloads overlap.
		for i := range leechers {
			lmd, err := torrent.NewFromInfo(info)
			require.NoError(t, err)

			tclient := torrenttestx.QuickClient(
				t,
				torrent.ClientConfigKeepAlive(time.Second),
				torrent.ClientConfigConnectionClosed(func(_ int160.T, stats torrent.ConnStats, _ int) {
					uploaded.Add(stats.BytesWrittenData.Int64())
					downloaded.Add(stats.BytesReadData.Int64())
				}),
			)
			defer tclient.Close()

			dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
			require.NoError(t, err)
			require.True(t, added)

			clients[i] = tclient

			dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			results[i] = make(chan result, 1)
			go func() {
				actual := md5.New()
				n, err := torrent.DownloadInto(dctx, actual, dl)
				results[i] <- result{n: n, digest: actual.Sum(nil), err: err}
			}()
		}

		for i := range leechers {
			res := <-results[i]
			require.NoError(t, res.err, "leecher %d: download failed", i)
			require.EqualValues(t, torrentlen, res.n, "leecher %d", i)
			require.Equal(t, expected.Sum(nil), res.digest, "leecher %d: digest mismatch", i)
		}

		// every leecher shares the same in-memory torrent, it was loaded exactly once.
		resident = 0
		require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
			resident++
			return false
		})))
		require.Equal(t, 1, resident, "seeder must serve all leechers from a single in-memory torrent")

		// Stop only unloads, the metadata stays on disk so any peer that dials the leecher
		// (the seeder and the other leechers all learn each other's addresses) reloads it.
		// closing the client shuts its listeners so the leechers really go away.
		for i := range leechers {
			require.NoError(t, clients[i].Close(), "leecher %d", i)
		}

		require.Eventually(t, func() bool {
			resident := 0
			require.NoError(t, sclient.Tune(torrent.ClientOperationClearIdleTorrents(func(torrent.Stats) bool {
				resident++
				return false
			})))
			return resident == 0
		}, 10*idletimeout, idletimeout/10, "seeder did not unload the idle torrent")

		// every client records both directions of its connections. the leechers may trade pieces with each other,
		// so the totals across all of the clients must balance rather than the seeder's uploads alone.
		require.Eventually(t, func() bool {
			return uploaded.Load() >= leechers*int64(torrentlen) && uploaded.Load() == downloaded.Load()
		}, 10*idletimeout, idletimeout/10, "uploaded %d and downloaded %d must agree and cover %d leechers of %d bytes", uploaded.Load(), downloaded.Load(), leechers, int64(torrentlen))
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
