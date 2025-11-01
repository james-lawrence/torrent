package torrent_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/require"
)

func TestMetadataExtension(t *testing.T) {
	// ensure a client with just the magnet link can bootstrap the info data
	const datan = 32 * bytesx.MiB

	ctx, done := testx.ContextWithTimeout(t, 1*time.Second)
	defer done()

	seedingdir := t.TempDir()
	mds := torrent.NewMetadataCache(seedingdir)
	sstore := storage.NewFile(seedingdir)
	info, _, err := testutil.RandomDataTorrent(seedingdir, datan)
	require.NoError(t, err)

	md, err := torrent.NewFromInfo(
		info,
		torrent.OptionDisplayName("test torrent"),
		torrent.OptionChunk(bytesx.KiB),
		torrent.OptionStorage(sstore),
	)
	require.NoError(t, err)
	require.NoError(t, mds.Write(md))
	require.NoError(t, testx.Touch(filepath.Join(seedingdir, md.ID.String())))

	// Create seeder and a Torrent.
	cfg := torrent.TestingConfig(
		t,
		seedingdir,
		torrent.ClientConfigSeed(true),
	)

	seeder, err := autobind.NewLoopback(autobind.DisableIPv6, autobind.DisableUTP).Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer seeder.Close()

	// Create leecher and a Torrent.
	cfg = torrent.TestingConfig(
		t,
		t.TempDir(),
		torrent.ClientConfigSeed(false),
	)

	c, err := torrent.NewClient(cfg)
	// we limit ourselves to tcp ipv4 here because of racing dialer.
	// basically the receiving end will see multiple connections but only one
	// will work. and the delay causes this test to take longer than it should.
	leecher, err := autobind.NewLoopback(autobind.DisableIPv6, autobind.DisableUTP).Bind(c, err)
	require.NoError(t, err)
	defer leecher.Close()

	mdl, err := torrent.NewFromMagnet(torrent.NewMagnet(md).String())
	require.NoError(t, err)

	linfo, err := c.Info(ctx, mdl, torrent.TuneClientPeer(seeder))
	require.NoError(t, err)
	encoded, err := metainfo.Encode(linfo)
	require.NoError(t, err)

	require.Equal(t, md.InfoBytes, encoded)
	require.Equal(t, md.ID, int160.FromHashedBytes(encoded))
}
