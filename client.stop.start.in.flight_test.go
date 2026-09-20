package torrent_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/james-lawrence/torrent/torrenttestx"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// TestClientStopStartInflight reproduces what a service does when a user pauses and later resumes a
// download: Client.Stop drops the torrent and Client.Start brings it back. requests that were outstanding
// when the transfer was interrupted must be handed back to the pool, a chunk left inflight is never
// requested again (outside of endgame only missing chunks are requested) so its piece can never complete
// and the transfer stalls. the client logs `still expecting N requests` each time a connection closes while
// requests are still outstanding.
func TestClientStopStartInflight(t *testing.T) {
	const torrentlen = 2 * bytesx.MiB

	t.Run("stopping a torrent unblocks the download in progress", func(t *testing.T) {
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

		// slow the leecher so the transfer is still in progress when it is stopped.
		tclient := torrenttestx.QuickClient(t, torrent.ClientConfigDownloadLimit(rate.NewLimiter(rate.Limit(256*bytesx.KiB), 64*bytesx.KiB)))
		defer tclient.Close()

		lmd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
		require.NoError(t, err)
		require.True(t, added)

		stopped := make(chan error, 1)
		go func() {
			_, err := torrent.DownloadInto(ctx, io.Discard, dl)
			stopped <- err
		}()

		require.Eventually(t, func() bool {
			s := dl.Stats()
			return s.DownloadedOptimistic > 0 && s.Downloaded < torrentlen
		}, 10*time.Second, 10*time.Millisecond, "the transfer must be partially complete before it is stopped")

		require.NoError(t, tclient.Stop(lmd))

		// callers (retrovibed's pause) wait on this goroutine to release the per download state.
		select {
		case err := <-stopped:
			require.Error(t, err, "a stopped torrent cannot complete the download")
		case <-time.After(10 * time.Second):
			require.Fail(t, "the download of a stopped torrent never returned")
		}
	})

	t.Run("stopping and starting the downloading torrent completes the transfer", func(t *testing.T) {
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

		tclient := torrenttestx.QuickClient(t, torrent.ClientConfigDownloadLimit(rate.NewLimiter(rate.Limit(256*bytesx.KiB), 64*bytesx.KiB)))
		defer tclient.Close()

		lmd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
		require.NoError(t, err)
		require.True(t, added)

		// the first download is abandoned by the pause, it is not waited on here, see the test above.
		go func() { _, _ = torrent.DownloadInto(ctx, io.Discard, dl) }()

		require.Eventually(t, func() bool {
			s := dl.Stats()
			return s.DownloadedOptimistic > 0 && s.Downloaded < torrentlen
		}, 10*time.Second, 10*time.Millisecond, "the transfer must be partially complete before it is stopped")

		// pause.
		require.NoError(t, tclient.Stop(lmd))

		// resume.
		dl, added, err = tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
		require.NoError(t, err)
		require.True(t, added, "a stopped torrent must be added again when it is started")

		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		second := make(chan error, 1)
		go func() {
			n, err := torrent.DownloadInto(dctx, io.Discard, dl)
			if err == nil && n != torrentlen {
				err = io.ErrShortWrite
			}
			second <- err
		}()

		// with no connection holding a request there can be no request outstanding, the client releases
		// them as connections close so allow it a moment to do so.
		var stalled time.Time
		for tick := time.NewTicker(50 * time.Millisecond); ; {
			select {
			case err := <-second:
				require.NoError(t, err, "the resumed transfer must complete")
				s := dl.Stats()
				require.EqualValues(t, torrentlen, s.Downloaded)
				require.Zero(t, s.Outstanding, "a completed transfer cannot have requests outstanding")
				return
			case <-tick.C:
				s := dl.Stats()
				if s.ActivePeers > 0 || s.Outstanding == 0 {
					stalled = time.Time{}
					continue
				}

				if stalled.IsZero() {
					stalled = time.Now()
				}

				require.Less(t, time.Since(stalled), 3*time.Second, "%d requests stay outstanding with no connection holding them, downloaded %d of %d", s.Outstanding, s.Downloaded, torrentlen)
			}
		}
	})

	t.Run("dropping every connection mid transfer releases every request", func(t *testing.T) {
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

		tclient := torrenttestx.QuickClient(t, torrent.ClientConfigDownloadLimit(rate.NewLimiter(rate.Limit(256*bytesx.KiB), 64*bytesx.KiB)))
		defer tclient.Close()

		lmd, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		dl, added, err := tclient.Start(lmd, torrent.TuneClientPeer(sclient), torrent.TuneNewConns)
		require.NoError(t, err)
		require.True(t, added)

		go func() { _, _ = torrent.DownloadInto(ctx, io.Discard, dl) }()

		require.Eventually(t, func() bool {
			s := dl.Stats()
			return s.DownloadedOptimistic > 0 && s.Downloaded < torrentlen
		}, 10*time.Second, 10*time.Millisecond, "the transfer must be partially complete before the seeder is stopped")

		// the seeder goes away mid transfer, dropping every connection of the leecher.
		require.NoError(t, sclient.Stop(smd))

		require.Eventually(t, func() bool {
			s := dl.Stats()
			return s.ActivePeers == 0 && s.Outstanding == 0
		}, 5*time.Second, 10*time.Millisecond, "every request must be released once the connections are gone: %+v", dl.Stats())
	})
}
