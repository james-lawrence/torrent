package torrent_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttestx"
)

// TuneDownloadRange allows downloading a subset of a torrent, these tests use it to
// pin that a torrent reports seeding as soon as it has readable data, it does not
// wait for the whole torrent to be downloaded.
func TestTuneDownloadRange(t *testing.T) {
	const (
		datan  = 64 * bytesx.KiB
		piecen = 16 * bytesx.KiB
	)

	ctx, done := testx.Context(t)
	defer done()

	seeddir := t.TempDir()
	info, _, err := testutil.RandomDataTorrent(seeddir, datan, metainfo.OptionPieceLength(piecen))
	require.NoError(t, err)
	require.EqualValues(t, 4, info.NumPieces())

	sstore := storage.NewFile(seeddir)
	defer sstore.Close()

	smd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(sstore))
	require.NoError(t, err)

	seeder, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, seeddir, torrent.ClientConfigSeed(true))))
	require.NoError(t, err)
	defer seeder.Close()

	seeded, _, err := seeder.Start(smd, torrent.TuneSeeding)
	require.NoError(t, err)
	require.NoError(t, torrent.Verify(ctx, seeded))

	t.Run("seeding enabled leecher reports seeding with a partial download", func(t *testing.T) {
		leechdir := t.TempDir()
		lstore := storage.NewFile(leechdir)
		defer lstore.Close()

		lmd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(lstore))
		require.NoError(t, err)

		leecher, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, _, err := leecher.Start(lmd)
		require.NoError(t, err)
		require.False(t, leeched.Stats().Seeding, "nothing downloaded yet, must not be seeding")

		// only the first piece is downloaded.
		require.NoError(t, leeched.Tune(torrent.TuneDownloadRange(0, piecen-1), torrent.TuneClientPeer(seeder)))

		require.Eventually(t, func() bool {
			return leeched.Stats().Completed == 1
		}, 10*time.Second, 10*time.Millisecond, "first piece was never completed")

		stats := leeched.Stats()
		require.True(t, stats.Seeding, "readable data available, must be seeding despite the incomplete download")
		require.Equal(t, 1, stats.Completed, "only a single piece should have been downloaded")
		require.Zero(t, stats.Missing)
		require.Zero(t, stats.Outstanding)
		require.Zero(t, stats.Unverified)
	})

	t.Run("seeding disabled leecher does not report seeding with a partial download", func(t *testing.T) {
		leechdir := t.TempDir()
		lstore := storage.NewFile(leechdir)
		defer lstore.Close()

		lmd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(lstore))
		require.NoError(t, err)

		leecher, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(false))))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, _, err := leecher.Start(lmd)
		require.NoError(t, err)
		require.False(t, leeched.Stats().Seeding)

		require.NoError(t, leeched.Tune(torrent.TuneDownloadRange(0, piecen-1), torrent.TuneClientPeer(seeder)))

		require.Eventually(t, func() bool {
			return leeched.Stats().Completed == 1
		}, 10*time.Second, 10*time.Millisecond, "first piece was never completed")

		stats := leeched.Stats()
		require.False(t, stats.Seeding, "seeding is disabled, must not be seeding")
		require.Equal(t, 1, stats.Completed)
	})
}
