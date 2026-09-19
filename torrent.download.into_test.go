package torrent_test

import (
	"crypto/md5"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/md5x"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttestx"
)

// DownloadInto is what marks a torrent as complete, these tests ensure the seeding
// status reported by the torrent follows the completion of the download.
func TestDownloadInto(t *testing.T) {
	const datan = 64 * bytesx.KiB

	ctx, done := testx.Context(t)
	defer done()

	seeddir := t.TempDir()
	info, expected, err := testutil.RandomDataTorrent(seeddir, datan)
	require.NoError(t, err)

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
	require.True(t, seeded.Stats().Seeding)

	t.Run("seeding enabled leecher reports seeding once complete", func(t *testing.T) {
		leechdir := t.TempDir()
		lstore := storage.NewFile(leechdir)
		defer lstore.Close()

		lmd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(lstore))
		require.NoError(t, err)

		leecher, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, added, err := leecher.Start(lmd, torrent.TuneClientPeer(seeder))
		require.NoError(t, err)
		require.True(t, added)

		require.False(t, leeched.Stats().Seeding, "nothing downloaded yet, must not be seeding")

		downloaded := md5.New()
		n, err := torrent.DownloadInto(ctx, downloaded, leeched)
		require.NoError(t, err)
		require.Equal(t, int64(datan), n)
		require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(downloaded))

		stats := leeched.Stats()
		require.True(t, stats.Seeding, "completed download with seeding enabled must be seeding")
		require.Zero(t, stats.Missing)
		require.Zero(t, stats.Outstanding)
		require.Zero(t, stats.Unverified)
	})

	t.Run("seeding disabled leecher never reports seeding", func(t *testing.T) {
		leechdir := t.TempDir()
		lstore := storage.NewFile(leechdir)
		defer lstore.Close()

		lmd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(lstore))
		require.NoError(t, err)

		leecher, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(false))))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, added, err := leecher.Start(lmd, torrent.TuneClientPeer(seeder))
		require.NoError(t, err)
		require.True(t, added)

		require.False(t, leeched.Stats().Seeding)

		downloaded := md5.New()
		n, err := torrent.DownloadInto(ctx, downloaded, leeched)
		require.NoError(t, err)
		require.Equal(t, int64(datan), n)
		require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(downloaded))

		stats := leeched.Stats()
		require.False(t, stats.Seeding, "seeding is disabled, completing must not enable it")
		require.Zero(t, stats.Missing)
	})

	t.Run("completed leecher serves a second leecher without the original seeder", func(t *testing.T) {
		origindir := t.TempDir()
		origininfo, originexpected, err := testutil.RandomDataTorrent(origindir, datan)
		require.NoError(t, err)

		ostore := storage.NewFile(origindir)
		defer ostore.Close()

		omd, err := torrent.NewFromInfo(origininfo, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(ostore))
		require.NoError(t, err)

		origin, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, origindir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)

		originated, _, err := origin.Start(omd, torrent.TuneSeeding)
		require.NoError(t, err)
		require.NoError(t, torrent.Verify(ctx, originated))

		adir := t.TempDir()
		astore := storage.NewFile(adir)
		defer astore.Close()

		amd, err := torrent.NewFromInfo(origininfo, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(astore))
		require.NoError(t, err)

		a, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, adir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer a.Close()

		atorrent, _, err := a.Start(amd, torrent.TuneClientPeer(origin))
		require.NoError(t, err)

		adownloaded := md5.New()
		_, err = torrent.DownloadInto(ctx, adownloaded, atorrent)
		require.NoError(t, err)
		require.Equal(t, md5x.FormatHex(originexpected), md5x.FormatHex(adownloaded))
		require.True(t, atorrent.Stats().Seeding)

		// the only remaining source of data is the first leecher.
		require.NoError(t, origin.Close())

		bdir := t.TempDir()
		bstore := storage.NewFile(bdir)
		defer bstore.Close()

		bmd, err := torrent.NewFromInfo(origininfo, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(bstore))
		require.NoError(t, err)

		b, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, bdir, torrent.ClientConfigSeed(false))))
		require.NoError(t, err)
		defer b.Close()

		btorrent, _, err := b.Start(bmd, torrent.TuneClientPeer(a))
		require.NoError(t, err)

		bdownloaded := md5.New()
		n, err := torrent.DownloadInto(ctx, bdownloaded, btorrent)
		require.NoError(t, err)
		require.Equal(t, int64(datan), n)
		require.Equal(t, md5x.FormatHex(originexpected), md5x.FormatHex(bdownloaded))
	})

	t.Run("seeding status is stable after completion", func(t *testing.T) {
		leechdir := t.TempDir()
		lstore := storage.NewFile(leechdir)
		defer lstore.Close()

		lmd, err := torrent.NewFromInfo(info, torrent.OptionChunk(bytesx.KiB), torrent.OptionStorage(lstore))
		require.NoError(t, err)

		leecher, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, _, err := leecher.Start(lmd, torrent.TuneClientPeer(seeder))
		require.NoError(t, err)

		downloaded := md5.New()
		_, err = torrent.DownloadInto(ctx, downloaded, leeched)
		require.NoError(t, err)
		require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(downloaded))

		require.True(t, leeched.Stats().Seeding)
		require.True(t, leeched.Stats().Seeding)

		require.NoError(t, leeched.Tune(torrent.TuneComplete))
		require.True(t, leeched.Stats().Seeding)
	})
}
