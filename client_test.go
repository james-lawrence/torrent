package torrent_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/dht/int160"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/internal/md5x"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
)

func TestingSeedConfig(t *testing.T, dir string, options ...torrent.ClientConfigOption) *torrent.ClientConfig {
	cfg := torrent.TestingConfig(
		t,
		dir,
		torrent.ClientConfigSeed(true),
		torrent.ClientConfigCompose(options...),
	)
	return cfg
}

func TestingLeechConfig(t *testing.T, dir string) *torrent.ClientConfig {
	cfg := torrent.TestingConfig(
		t,
		dir,
		torrent.ClientConfigSeed(true),
	)
	return cfg
}

func TestClientDefault(t *testing.T) {
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	cl.Close()
}

func TestClientNilConfig(t *testing.T) {
	cl, err := torrent.NewClient(nil)
	require.NoError(t, err)
	cl.Close()
}

func TestBoltPieceCompletionClosedWhenClientClosed(t *testing.T) {
	cfg := torrent.TestingConfig(t, t.TempDir())
	// ci := storage.NewFile(cfg.DataDir)
	// defer ci.Close()

	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	cl.Close()
	// And again, https://github.com/anacrolix/torrent/issues/158
	cl, err = autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	cl.Close()
}

func TestAddDropTorrent(t *testing.T) {
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()
	dir := t.TempDir()
	mi := testutil.GreetingTestTorrent(dir)

	tt, added, err := cl.MaybeStart(torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewFile(dir))))
	require.NoError(t, err)
	assert.True(t, added)

	require.NoError(t, tt.Tune(torrent.TuneMaxConnections(0)))
	require.NoError(t, tt.Tune(torrent.TuneMaxConnections(1)))

	cl.Stop(tt.Metadata())
}

func TestAddDropManyTorrents(t *testing.T) {
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()
	for i := range 1000 {
		var spec torrent.Metadata
		binary.PutVarint((&spec).ID[:], int64(i))
		_, added, err := cl.Start(spec)
		require.NoError(t, err)
		assert.True(t, added)
		defer cl.Stop(spec)
	}
}

func NewFileCacheClientStorageFactory(dataDir string) storage.ClientImpl {
	return storage.NewFile(dataDir)
}

type StorageFactory func(string) storage.ClientImpl

func TestClientTransferRateLimitedUpload(t *testing.T) {
	started := time.Now()
	testClientTransfer(t, testClientTransferParams{
		// We are uploading 13 bytes (the length of the greeting torrent). The
		// chunks are 2 bytes in length. Then the smallest burst we can run
		// with is 2. Time taken is (13-burst)/rate.
		SeederUploadRateLimiter: rate.NewLimiter(11, 2),
		ExportClientStatus:      true,
	})
	require.True(t, time.Since(started) > time.Second)
}

func TestClientTransferRateLimitedDownload(t *testing.T) {
	testClientTransfer(t, testClientTransferParams{
		LeecherDownloadRateLimiter: rate.NewLimiter(512, 512),
	})
}

func TestClientTransferVarious(t *testing.T) {
	// Leecher storage
	for _, ls := range []StorageFactory{
		func(dir string) storage.ClientImpl {
			return storage.NewFile(dir)
		},
	} {
		// Seeder storage
		for _, ss := range []func(string) storage.ClientImpl{
			func(dir string) storage.ClientImpl {
				return storage.NewFile(dir)
			},
			storage.NewMMap,
		} {
			testClientTransfer(t, testClientTransferParams{
				SeederStorage:  ss,
				LeecherStorage: ls,
			})
		}
	}
}

type testClientTransferParams struct {
	ExportClientStatus         bool
	LeecherStorage             func(string) storage.ClientImpl
	SeederStorage              func(string) storage.ClientImpl
	SeederUploadRateLimiter    *rate.Limiter
	LeecherDownloadRateLimiter *rate.Limiter
}

func TestClientTransferDefault(t *testing.T) {
	testClientTransfer(t, testClientTransferParams{
		ExportClientStatus: true,
		LeecherStorage: func(dir string) storage.ClientImpl {
			return storage.NewFile(dir)
		},
	})
}

// Creates a seeder and a leecher, and ensures the data transfers when a read
// is attempted on the leecher.
func testClientTransfer(t *testing.T, ps testClientTransferParams) {
	ctx, done := testx.Context(t)
	defer done()

	greetingTempDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(greetingTempDir)

	// Create seeder and a Torrent.
	cfg := torrent.TestingConfig(t, greetingTempDir, torrent.ClientConfigSeed(true))
	if ps.SeederUploadRateLimiter != nil {
		cfg.UploadRateLimiter = ps.SeederUploadRateLimiter
	}

	sstore := storage.NewFile(greetingTempDir)
	storageopt := torrent.OptionStorage(sstore)
	if ps.SeederStorage != nil {
		sstore.Close()
		store := ps.SeederStorage(greetingTempDir)
		defer store.Close()
		storageopt = torrent.OptionStorage(store)
	}

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)

	md, err := torrent.NewFromMetaInfo(mi, storageopt)
	require.NoError(t, err)

	seederTorrent, _, err := seeder.Start(md)
	require.NoError(t, err)
	// Run a Stats right after Closing the Client. This will trigger the Stats
	// panic in #214 caused by RemoteAddr on Closed uTP sockets.
	defer seederTorrent.Stats()
	defer seeder.Close()

	require.NoError(t, torrent.Verify(ctx, seederTorrent))
	_, err = torrent.DownloadInto(ctx, io.Discard, seederTorrent, torrent.TuneSeeding)
	require.NoError(t, err)

	leechdir := t.TempDir()
	lstore := storage.NewFile(leechdir)
	// Create leecher and a Torrent.
	cfg = torrent.TestingConfig(
		t,
		leechdir,
		torrent.ClientConfigSeed(false),
	)

	leechstorageopt := torrent.OptionStorage(lstore)
	if ps.LeecherStorage != nil {
		lstore.Close()
		lstore := ps.LeecherStorage(leechdir)
		defer lstore.Close()
		leechstorageopt = torrent.OptionStorage(lstore)
	}

	if ps.LeecherDownloadRateLimiter != nil {
		cfg.DownloadRateLimiter = ps.LeecherDownloadRateLimiter
	}

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer leecher.Close()

	leecherTorrent, added, err := leecher.MaybeStart(
		torrent.NewFromMetaInfo(
			mi,
			torrent.OptionChunk(2),
			leechstorageopt,
		),
	)
	require.NoError(t, err)
	assert.True(t, added)

	// Now do some things with leecher and seeder.
	require.NoError(t, leecherTorrent.Tune(torrent.TuneClientPeer(seeder)))

	// The Torrent should not be interested in obtaining peers, so the one we
	// just added should be the only one.
	assert.False(t, leecherTorrent.Stats().Seeding)

	// begin downloading
	_, err = torrent.DownloadInto(ctx, io.Discard, leecherTorrent)
	require.NoError(t, err)

	r := torrent.NewReader(leecherTorrent)
	defer r.Close()

	assertReadAllGreeting(t, r)

	seederStats := seederTorrent.Stats()
	assert.True(t, 13 <= seederStats.BytesWrittenData.Int64())
	assert.True(t, 8 <= seederStats.ChunksWritten.Int64())

	leecherStats := leecherTorrent.Stats()
	assert.True(t, 13 <= leecherStats.BytesReadData.Int64())
	assert.True(t, 8 <= leecherStats.ChunksRead.Int64())

	// Try reading through again for the cases where the torrent data size
	// exceeds the size of the cache.
	assertReadAllGreeting(t, r)
}

func assertReadAllGreeting(t *testing.T, r io.ReadSeeker) {
	pos, err := r.Seek(0, io.SeekStart)
	assert.NoError(t, err)
	assert.EqualValues(t, 0, pos)
	_greeting, err := io.ReadAll(r)
	assert.NoError(t, err)
	assert.EqualValues(t, testutil.GreetingFileContents, string(_greeting))
}

func TestClientSeedWithoutAdding(t *testing.T) {
	t.Run("no encryption", func(t *testing.T) {
		const datan = 64 * bytesx.KiB

		ctx, done := testx.Context(t)
		defer done()

		seedingdir := t.TempDir()
		mds := torrent.NewMetadataCache(seedingdir)
		bms := torrent.NewBitmapCache(seedingdir)
		sstore := storage.NewFile(seedingdir)
		info, expected, err := testutil.RandomDataTorrent(seedingdir, datan)
		require.NoError(t, err)

		md, err := torrent.NewFromInfo(
			info,
			torrent.OptionDisplayName("test torrent"),
			torrent.OptionChunk(bytesx.KiB),
			torrent.OptionStorage(sstore),
		)
		require.NoError(t, err)
		require.NoError(t, mds.Write(md))
		require.NoError(t, bms.Write(md.Metainfo().ID(), bitmapx.Fill(uint64(info.PieceLength)/md.ChunkSize*info.NumPieces())))

		// Create seeder and a Torrent.
		cfg := torrent.TestingConfig(
			t,
			seedingdir,
			torrent.ClientConfigSeed(true),
		)

		seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
		require.NoError(t, err)
		defer seeder.Close()

		leechdir := t.TempDir()
		// Create leecher and a Torrent.
		cfg = torrent.TestingConfig(
			t,
			leechdir,
			torrent.ClientConfigSeed(false),
		)

		leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, added, err := leecher.Start(md, torrent.TuneClientPeer(seeder))
		require.NoError(t, err)
		assert.True(t, added)

		// The Torrent should not be interested in obtaining peers, so the one we
		// just added should be the only one.
		require.False(t, leeched.Stats().Seeding)

		// download
		downloaded := md5.New()
		n, err := torrent.DownloadInto(ctx, downloaded, leeched)
		require.NoError(t, err)
		require.Equal(t, int64(64*bytesx.KiB), n)
		require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(downloaded))
	})

	t.Run("with encryption", func(t *testing.T) {
		// this test ensures encryption works when torrents are not actively in memory.
		const datan = 64 * bytesx.KiB

		ctx, done := testx.Context(t)
		defer done()

		seedingdir := t.TempDir()
		mds := torrent.NewMetadataCache(seedingdir)
		sstore := storage.NewFile(seedingdir)
		info, expected, err := testutil.RandomDataTorrent(seedingdir, datan)
		require.NoError(t, err)

		md, err := torrent.NewFromInfo(
			info,
			torrent.OptionDisplayName("test torrent"),
			torrent.OptionChunk(bytesx.KiB),
			torrent.OptionStorage(sstore),
		)
		require.NoError(t, err)
		require.NoError(t, mds.Write(md))

		// Create seeder and a Torrent.
		cfg := torrent.TestingConfig(
			t,
			seedingdir,
			torrent.ClientConfigSeed(true),
			torrent.ClientConfigEnableEncryption,
		)

		seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
		require.NoError(t, err)
		defer seeder.Close()

		// Create leecher and a Torrent.
		cfg = torrent.TestingConfig(
			t,
			t.TempDir(),
			torrent.ClientConfigSeed(false),
			torrent.ClientConfigEnableEncryption,
		)

		leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
		require.NoError(t, err)
		defer leecher.Close()

		leeched, added, err := leecher.Start(md, torrent.TuneClientPeer(seeder))
		require.NoError(t, err)
		assert.True(t, added)

		// The Torrent should not be interested in obtaining peers, so the one we
		// just added should be the only one.
		require.False(t, leeched.Stats().Seeding)

		// download
		downloaded := md5.New()
		n, err := torrent.DownloadInto(ctx, downloaded, leeched)
		require.NoError(t, err)
		require.Equal(t, int64(64*bytesx.KiB), n)
		require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(downloaded))
	})
}

// Check that after completing leeching, a leecher transitions to a seeding
// correctly. Connected in a chain like so: Seeder <-> Leecher <-> LeecherLeecher.
func TestSeedAfterDownloading(t *testing.T) {
	ctx, _done := testx.Context(t)
	defer _done()

	greetingTempDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(greetingTempDir)

	cfg := torrent.TestingConfig(
		t,
		greetingTempDir,
		torrent.ClientConfigSeed(true),
	)

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer seeder.Close()

	seederTorrent, ok, err := seeder.MaybeStart(torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewFile(greetingTempDir))))
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = torrent.DownloadInto(ctx, io.Discard, seederTorrent, torrent.TuneSeeding)
	require.NoError(t, err)

	lcfg := torrent.TestingConfig(
		t,
		t.TempDir(),
		torrent.ClientConfigSeed(true),
	)

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(lcfg))
	require.NoError(t, err)
	defer leecher.Close()

	lleecherdir := t.TempDir()
	llcfg := torrent.TestingConfig(
		t,
		lleecherdir,
		torrent.ClientConfigSeed(false),
	)

	leecherLeecher, _ := autobind.NewLoopback().Bind(torrent.NewClient(llcfg))
	require.NoError(t, err)
	defer leecherLeecher.Close()
	leecherGreeting, ok, err := leecher.MaybeStart(torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2)))
	require.NoError(t, err)
	assert.True(t, ok)

	llg, ok, err := leecherLeecher.MaybeStart(torrent.NewFromMetaInfo(mi, torrent.OptionChunk(3)))
	require.NoError(t, err)
	assert.True(t, ok)

	// Simultaneously in Leecher, and read the contents
	// consecutively in LeecherLeecher. This non-deterministically triggered a
	// case where the leecher wouldn't unchoke the LeecherLeecher.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		_, err = torrent.DownloadInto(ctx, &buf, llg)
		require.NoError(t, err)
		assert.EqualValues(t, testutil.GreetingFileContents, buf.Bytes())
	}()
	done := make(chan struct{})
	defer close(done)
	go leecherGreeting.Tune(torrent.TuneClientPeer(seeder))
	go leecherGreeting.Tune(torrent.TuneClientPeer(leecherLeecher))

	digest := md5.New()
	n, err := torrent.DownloadInto(ctx, digest, leecherGreeting)
	require.Equal(t, int64(13), n)
	require.NoError(t, err)
	require.Equal(t, "22c3683b094136c3398391ae71b20f04", md5x.FormatHex(digest))
}

func TestDownload(t *testing.T) {
	buf := bytes.NewBufferString("")

	greetingTempDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(greetingTempDir)

	metadata, err := torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewFile(greetingTempDir)))
	require.NoError(t, err)

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, greetingTempDir)))
	require.NoError(t, err)
	defer seeder.Close()

	tor, added, err := seeder.Start(metadata, torrent.TuneVerifyFull)
	require.NoError(t, err)
	require.True(t, added)
	_, err = torrent.DownloadInto(t.Context(), io.Discard, tor)
	require.NoError(t, err)

	lcfg := TestingLeechConfig(t, t.TempDir())
	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(lcfg))
	require.NoError(t, err)
	defer leecher.Close()

	metadata, err = torrent.NewFromMetaInfo(mi)
	require.NoError(t, err)
	ltor, added, err := leecher.Start(metadata)
	require.NoError(t, err)
	require.True(t, added)
	_, err = torrent.DownloadInto(t.Context(), buf, ltor, torrent.TuneClientPeer(seeder))
	require.NoError(t, err)
	require.Equal(t, "hello, world\n", buf.String())
}

func TestDownloadMetadataTimeout(t *testing.T) {
	buf := bytes.NewBufferString("")
	greetingTempDir := t.TempDir()

	mi := testutil.GreetingTestTorrent(greetingTempDir)
	metadata, err := torrent.New(mi.HashInfoBytes())
	require.NoError(t, err)

	cfg := torrent.TestingConfig(t, t.TempDir())
	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer leecher.Close()
	ctx, done := context.WithTimeout(t.Context(), 0)
	defer done()
	ltor, added, err := leecher.Start(metadata)
	require.NoError(t, err)
	require.True(t, added)
	_, err = torrent.DownloadInto(ctx, buf, ltor)
	require.Equal(t, context.DeadlineExceeded, err)
}

func TestCompletedPieceWrongSize(t *testing.T) {
	cfg := torrent.TestingConfig(t, t.TempDir())
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer cl.Close()
	info := metainfo.Info{
		Length:      13,
		PieceLength: 15,
		Pieces:      make([]byte, 20),
	}

	b, err := bencode.Marshal(info)
	require.NoError(t, err)
	ts, err := torrent.New(metainfo.NewHashFromBytes(b), torrent.OptionInfo(b), torrent.OptionStorage(testutil.NewBadStorage()))
	require.NoError(t, err)
	tt, new, err := cl.Start(ts, torrent.TuneVerifyFull)
	require.NoError(t, err)
	defer cl.Stop(ts)
	assert.True(t, new)
	require.Equal(t, 1, tt.Stats().Missing)
}

func BenchmarkAddLargeTorrent(b *testing.B) {
	cfg := torrent.TestingConfig(b, b.TempDir())
	cl, err := autobind.NewLoopback(
		autobind.DisableTCP,
		autobind.DisableUTP,
	).Bind(torrent.NewClient(cfg))
	require.NoError(b, err)
	defer cl.Close()
	b.ReportAllocs()
	for range b.N {
		t, err := torrent.NewFromMetaInfoFile("testdata/bootstrap.dat.torrent")
		if err != nil {
			b.Fatal(err)
		}

		_, _, err = cl.Start(t)
		if err != nil {
			b.Fatal(err)
		}
		cl.Stop(t)
	}
}

func TestResponsive(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()

	seederDataDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(seederDataDir)
	cfg := torrent.TestingConfig(
		t,
		seederDataDir,
		torrent.ClientConfigSeed(true),
	)

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.Nil(t, err)
	defer seeder.Close()
	tt, err := torrent.NewFromMetaInfo(mi)
	// tt, err := torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewFile(cfg.DataDir)))
	require.Nil(t, err)
	seederTorrent, _, _ := seeder.Start(tt)
	require.NoError(t, torrent.Verify(ctx, seederTorrent))

	cfg = torrent.TestingConfig(t, t.TempDir())
	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer leecher.Close()
	tt, err = torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2))
	require.NoError(t, err)
	leecherTorrent, _, err := leecher.Start(tt, torrent.TuneClientPeer(seeder))
	require.NoError(t, err)

	_, err = torrent.DownloadInto(ctx, io.Discard, leecherTorrent)
	require.NoError(t, err)

	reader := torrent.NewReader(leecherTorrent)
	defer reader.Close()

	b := make([]byte, 2)
	_, err = reader.Seek(3, io.SeekStart)
	require.NoError(t, err)
	_, err = io.ReadFull(reader, b)
	assert.Nil(t, err)
	assert.EqualValues(t, "lo", string(b))
	_, err = reader.Seek(11, io.SeekStart)
	require.NoError(t, err)
	n, err := io.ReadFull(reader, b)
	assert.Nil(t, err)
	assert.EqualValues(t, 2, n)
	assert.EqualValues(t, "d\n", string(b))
}

func TestTorrentDroppedDuringResponsiveRead(t *testing.T) {
	t.SkipNow()
	seederDataDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(seederDataDir)

	cfg := torrent.TestingConfig(
		t,
		seederDataDir,
		torrent.ClientConfigSeed(true),
	)

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer seeder.Close()
	st, err := torrent.NewFromMetaInfo(mi)
	require.NoError(t, err)

	_, _, err = seeder.Start(st, torrent.TuneVerifyFull)
	require.NoError(t, err)

	leecherDataDir := t.TempDir()
	cfg = torrent.TestingConfig(
		t,
		leecherDataDir,
	)

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer leecher.Close()
	lt, err := torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2))
	// lt, err := torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2), torrent.OptionStorage(storage.NewFile(leecherDataDir)))
	// lt, err := torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2), torrent.OptionStorage(storage.NewFile(cfg.DataDir)))
	require.NoError(t, err)
	leecherTorrent, _, err := leecher.Start(lt, torrent.TuneAutoDownload, torrent.TuneClientPeer(seeder))
	require.NoError(t, err)

	reader := torrent.NewReader(leecherTorrent)
	defer reader.Close()

	b := make([]byte, 2)
	_, err = reader.Seek(3, io.SeekStart)
	require.NoError(t, err)
	_, err = io.ReadFull(reader, b)
	assert.Nil(t, err)
	assert.EqualValues(t, "lo", string(b))
	leecher.Stop(lt)
	_, err = reader.Seek(11, io.SeekStart)
	require.NoError(t, err)
	n, err := reader.Read(b)
	assert.EqualError(t, err, "storage closed")
	assert.EqualValues(t, 0, n)
}

// Check that stuff is merged in subsequent Start for the same
// infohash.
func TestAddTorrentMerging(t *testing.T) {
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()

	dir := t.TempDir()

	mi := testutil.GreetingTestTorrent(dir)
	defer os.RemoveAll(dir)
	ts, err := torrent.NewFromMetaInfo(mi, torrent.OptionDisplayName("foo"), torrent.OptionStorage(storage.NewFile(dir)))

	require.NoError(t, err)
	tt, added, err := cl.Start(ts)
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "foo", tt.Metadata().DisplayName)
	_, added, err = cl.Start(ts.Merge(torrent.OptionDisplayName("bar")))
	require.NoError(t, err)
	require.False(t, added)
	assert.Equal(t, "bar", tt.Metadata().DisplayName)
}

func TestTorrentDroppedBeforeGotInfo(t *testing.T) {
	dir := t.TempDir()
	mi := testutil.GreetingTestTorrent(dir)
	os.RemoveAll(dir)
	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()
	ts, err := torrent.New(mi.HashInfoBytes())
	require.NoError(t, err)
	tt, added, err := cl.Start(ts)
	require.NoError(t, err)
	require.True(t, added)
	cl.Stop(ts)

	select {
	case <-tt.GotInfo():
		t.FailNow()
	default:
	}
}

func TestTorrentDownloadAll(t *testing.T) {
	torrent.DownloadCancelTest(t, autobind.NewLoopback(), autobind.NewLoopback(), torrent.TestDownloadCancelParams{})
}

func TestTorrentDownloadAllThenCancel(t *testing.T) {
	torrent.DownloadCancelTest(t, autobind.NewLoopback(), autobind.NewLoopback(), torrent.TestDownloadCancelParams{
		Cancel: true,
	})
}

func TestPieceCompletedInStorageButNotClient(t *testing.T) {
	greetingTempDir := t.TempDir()
	greetingMetainfo := testutil.GreetingTestTorrent(greetingTempDir)

	cfg := torrent.TestingConfig(
		t,
		greetingTempDir,
	)

	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	ts, err := torrent.NewFromMetaInfo(greetingMetainfo, torrent.OptionStorage(storage.NewFile(greetingTempDir)))
	require.NoError(t, err)
	_, _, err = seeder.Start(ts)
	require.NoError(t, err)
}

func TestClientDynamicListenTCPOnly(t *testing.T) {
	cfg := torrent.TestingConfig(t, t.TempDir())
	cl, err := autobind.NewLoopback(
		autobind.DisableUTP,
	).Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer cl.Close()
	assert.NotEqual(t, 0, cl.LocalPort())
}

func TestClientDynamicListenUTPOnly(t *testing.T) {
	t.Skip("disabled - until we resolve the utp dependency nonsense")
	cfg := torrent.TestingConfig(t, t.TempDir())
	cl, err := autobind.NewLoopback(
		autobind.DisableTCP,
	).Bind(torrent.NewClient(cfg))
	require.NoError(t, err)
	defer cl.Close()
	assert.NotEqual(t, 0, cl.LocalPort())
}

// Creates a file containing random data. Make a metainfo from that, adds it to the given
// client, and returns a magnet link.
func makeMagnet(t *testing.T, cl *torrent.Client, dir string, name string) string {
	ctx, done := testx.Context(t)
	defer done()

	os.MkdirAll(dir, 0770)
	file, err := os.Create(filepath.Join(dir, name))
	require.NoError(t, err)
	file.Write([]byte(name))
	file.Close()
	info, _, err := testutil.RandomDataTorrent(dir, bytesx.KiB)
	require.NoError(t, err)

	mi := metainfo.MetaInfo{}
	mi.SetDefaults()
	mi.InfoBytes, err = bencode.Marshal(info)
	require.NoError(t, err)
	magnet := mi.Magnet(name, mi.HashInfoBytes()).String()
	ts, err := torrent.NewFromMetaInfo(&mi)
	require.NoError(t, err)
	tr, _, err := cl.Start(ts, torrent.TuneSeeding)
	require.NoError(t, err)
	require.True(t, tr.Stats().Seeding)
	require.NoError(t, torrent.Verify(ctx, tr))
	return magnet
}

// https://github.com/anacrolix/torrent/issues/114
func TestMultipleTorrentsWithEncryption(t *testing.T) {
	testSeederLeecherPair(
		t,
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

// Test that the leecher can download a torrent in its entirety from the seeder. Note that the
// seeder config is done first.
func testSeederLeecherPair(t *testing.T, seeder func(*torrent.ClientConfig), leecher func(*torrent.ClientConfig)) {
	ctx, done := testx.Context(t)
	defer done()

	datadir := t.TempDir()

	scfg := torrent.TestingConfig(
		t,
		datadir,
		torrent.ClientConfigSeed(true),
	)

	scfg.Handshaker = connections.NewHandshaker(
		connections.NewFirewall(),
	)

	seeder(scfg)
	server, err := autobind.NewLoopback().Bind(torrent.NewClient(scfg))
	require.NoError(t, err)
	defer server.Close()

	magnet1 := makeMagnet(t, server, datadir, "test1")
	// Extra torrents are added to test the seeder having to match incoming obfuscated headers
	// against more than one torrent. See issue #114
	makeMagnet(t, server, datadir, "test2")

	for i := range 100 {
		makeMagnet(t, server, datadir, fmt.Sprintf("test%d", i+2))
	}

	lcfg := torrent.TestingConfig(
		t,
		t.TempDir(),
	)
	lcfg.Handshaker = connections.NewHandshaker(
		connections.NewFirewall(),
	)

	leecher(lcfg)
	client, err := autobind.NewLoopback().Bind(torrent.NewClient(lcfg))
	require.NoError(t, err)
	defer client.Close()

	ts, err := torrent.NewFromMagnet(magnet1)
	require.NoError(t, err)
	tr, _, err := client.Start(ts, torrent.TuneClientPeer(server))
	require.NoError(t, err)

	n, err := torrent.DownloadInto(ctx, io.Discard, tr)
	require.NoError(t, err)
	require.Equal(t, int64(1024), n)
}

// This appears to be the situation with the S3 BitTorrent client.
func TestObfuscatedHeaderFallbackSeederDisallowsLeecherPrefers(t *testing.T) {
	// Leecher prefers obfuscation, but the seeder does not allow it.
	testSeederLeecherPair(
		t,
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = false
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

func TestObfuscatedHeaderFallbackSeederRequiresLeecherPrefersNot(t *testing.T) {
	// Leecher prefers no obfuscation, but the seeder enforces it.
	testSeederLeecherPair(
		t,
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *torrent.ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = false
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

func TestRandomSizedTorrents(t *testing.T) {
	dir := t.TempDir()
	// this should be larger than a single piece length
	n := rand.Int64N(3 * bytesx.MiB)
	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, dir)))
	require.NoError(t, err)
	defer seeder.Close()

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingLeechConfig(t, testutil.Autodir(t))))
	require.NoError(t, err)
	defer leecher.Close()

	testTransferRandomData(t, dir, n, seeder, leecher)
}

func Test128KBTorrent(t *testing.T) {
	dir := t.TempDir()
	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, dir)))
	require.NoError(t, err)
	defer seeder.Close()

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingLeechConfig(t, testutil.Autodir(t))))
	require.NoError(t, err)
	defer leecher.Close()

	testTransferRandomData(t, dir, 128*bytesx.KiB, seeder, leecher)
}

func Test3MBPlus1Torrent(t *testing.T) {
	dir := t.TempDir()
	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, dir)))
	require.NoError(t, err)
	defer seeder.Close()

	leecher, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingLeechConfig(t, testutil.Autodir(t))))
	require.NoError(t, err)
	defer leecher.Close()

	testTransferRandomData(t, dir, 3*bytesx.MiB+1, seeder, leecher)
}

func TestDownloadRange(t *testing.T) {
	testRange := func(n, offset, length int64) func(t *testing.T) {
		return func(t *testing.T) {
			// log.Println(t.Name(), n, offset, length)
			datadir := t.TempDir()
			from, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, datadir)))
			require.NoError(t, err)
			defer from.Close()

			to, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingLeechConfig(t, t.TempDir())))
			require.NoError(t, err)
			defer to.Close()

			ctx, done := testx.Context(t)
			defer done()

			data, _, err := testutil.IOTorrent(datadir, cryptox.NewChaCha8(t.Name()), n)
			require.NoError(t, err)
			defer os.Remove(data.Name())

			metadata, err := torrent.NewFromFile(
				data.Name(),
				torrent.OptionStorage(storage.NewFile(datadir, storage.FileOptionPathMakerFixed(filepath.Base(data.Name())))),
			)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(datadir, metadata.ID.String()), 0700))

			dl0, added, err := from.Start(metadata, torrent.TuneVerifyFull)
			require.NoError(t, err)
			require.True(t, added)
			defer from.Stop(dl0.Metadata())

			metadata, err = torrent.NewFromFile(data.Name(), torrent.OptionStorage(storage.NewFile(t.TempDir())))
			require.NoError(t, err)

			digestdl := md5.New()

			expected := md5.New()
			n, err = io.Copy(expected, io.NewSectionReader(data, offset, length))
			require.NoError(t, err)
			require.EqualValues(t, length, n)

			dl1, added, err := to.Start(metadata)
			require.NoError(t, err)
			require.True(t, added)
			defer to.Stop(dl1.Metadata())
			// log.Printf("from %d -> to %d, offset %d length %d\n", from.LocalPort(), to.LocalPort(), offset, length)
			dl := torrent.DownloadRange(ctx, dl1, offset, length, torrent.TuneClientPeer(from))

			n, err := io.Copy(digestdl, dl)
			require.NoError(t, err)
			require.EqualValues(t, length, n)

			require.Equal(t, expected.Sum(nil), digestdl.Sum(nil), "digest mismatch: generated torrent length", n)
		}
	}

	t.Run("example.1", testRange(128*bytesx.KiB, 0, 32*bytesx.KiB))
	t.Run("example.2", testRange(128*bytesx.KiB, 16*bytesx.KiB, 16*bytesx.KiB))
	t.Run("example.3", testRange(128*bytesx.KiB, 16*bytesx.KiB, 112*bytesx.KiB))
	t.Run("example.4", testRange(128*bytesx.KiB, 0, 128*bytesx.KiB))
}

func TestTorrentPieceLengthMultipleOfTotalLength(t *testing.T) {
	// this test is important! it checks the case where a torrent total length is a multiple of the piece length exactly.
	// rare but possible. requires we set the amount of data equal to n * PieceLength.
	dir := t.TempDir()
	seeder, err := autobind.NewLoopback().Bind(torrent.NewClient(TestingSeedConfig(t, dir)))
	require.NoError(t, err)
	defer seeder.Close()

	leecher, err := autobind.NewLoopback(autobind.DisableUTP).Bind(torrent.NewClient(TestingLeechConfig(t, testutil.Autodir(t))))
	require.NoError(t, err)
	defer leecher.Close()

	testTransferRandomData(t, dir, 1*bytesx.MiB, seeder, leecher)
}

func testTransferRandomData(t *testing.T, datadir string, n int64, from, to *torrent.Client) {
	ctx, done := testx.Context(t)
	defer done()

	data, expected, err := testutil.IOTorrent(datadir, cryptox.NewChaCha8(t.Name()), n)
	require.NoError(t, err)
	defer os.Remove(data.Name())

	metadata, err := torrent.NewFromFile(
		data.Name(),
		torrent.OptionStorage(storage.NewFile(datadir, storage.FileOptionPathMakerFixed(filepath.Base(data.Name())))),
	)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(datadir, metadata.ID.String()), 0700))

	dl0, added, err := from.Start(metadata, torrent.TuneVerifyFull)
	require.NoError(t, err)
	require.True(t, added)
	defer from.Stop(dl0.Metadata())

	metadata, err = torrent.NewFromFile(data.Name(), torrent.OptionStorage(storage.NewFile(t.TempDir())))
	require.NoError(t, err)

	digestdl := md5.New()
	dl1, added, err := to.Start(metadata, torrent.TuneClientPeer(from), torrent.TuneNewConns)
	require.NoError(t, err)
	require.True(t, added)
	defer to.Stop(dl1.Metadata())

	dln, err := torrent.DownloadInto(ctx, digestdl, dl1)
	require.NoError(t, err)

	require.Equal(t, n, dln)
	require.Equal(t, expected.Sum(nil), digestdl.Sum(nil), "digest mismatch: generated torrent length", n)
}

func TestClientAddressInUse(t *testing.T) {
	s, err := net.Listen("tcp", ":50007")
	require.NoError(t, err)
	if s != nil {
		defer s.Close()
	}
	cl, err := autobind.NewSpecified(":50007").Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir())))
	require.Error(t, err)
	require.Nil(t, cl)
}

func TestClientHasDhtServersWhenUTPDisabled(t *testing.T) {
	t.Skip("disabled - until we resolve the utp dependency nonsense")
	cc := torrent.TestingConfig(t, t.TempDir())
	cl, err := autobind.NewLoopback(
		autobind.DisableUTP,
		autobind.EnableDHT,
	).Bind(torrent.NewClient(cc))
	require.NoError(t, err)
	defer cl.Close()
	require.NotEmpty(t, cl.DhtServers())
}

// func TestIssue335(t *testing.T) {
// 	dir, mi := testutil.GreetingTestTorrent(t)
// 	defer os.RemoveAll(dir)
// 	cfg := torrent.TestingConfig(t)
// 	cfg.Seed = false
// 	cfg.DataDir = dir

// 	cl, err := autobind.NewLoopback().Bind(torrent.NewClient(cfg))
// 	require.NoError(t, err)
// 	defer cl.Close()
// 	ts, err := torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewMMap(dir)))
// 	require.NoError(t, err)
// 	_, added, err := cl.Start(ts, torrent.TuneAutoDownload)
// 	require.NoError(t, err)
// 	assert.True(t, added)
// 	require.True(t, cl.WaitAll())
// 	require.NoError(t, cl.Stop(ts))
// 	_, added, err = cl.Start(ts, torrent.TuneAutoDownload)
// 	require.NoError(t, err)
// 	assert.True(t, added)
// 	require.True(t, cl.WaitAll())
// }

func TestProtocolSwarmManagement(t *testing.T) {
	// const iolimit int64 = 128 * bytesx.KiB

	t.Run("should maintain a peer even in face of networking issues", func(t *testing.T) {
		datadir := t.TempDir()
		from, err := autobind.NewLoopback().Bind(
			torrent.NewClient(
				TestingSeedConfig(
					t,
					datadir,
					torrent.ClientConfigHandshakeTimeout(200*time.Millisecond),
					torrent.ClientConfigDialTimeouts(20*time.Millisecond, 200*time.Millisecond),
				),
			),
		)
		require.NoError(t, err)
		defer from.Close()

		md, _ := TorrentFile(t, datadir, cryptox.NewChaCha8(t.Name()), 128*bytesx.KiB)

		seeding, _, err := from.Start(md)
		require.NoError(t, err)
		_ = seeding

		a := netip.AddrPortFrom(netip.IPv6Loopback(), 0)

		l, err := net.Listen("tcp6", a.String())
		require.NoError(t, err)
		defer l.Close()
		a, err = netip.ParseAddrPort(l.Addr().String())
		require.NoError(t, err)

		require.EqualValues(t, 0, seeding.Stats().ActivePeers)
		require.NoError(t, seeding.Tune(torrent.TunePeers(torrent.NewPeer(int160.Random(), a))))

		for range 5 {
			// log.Println("stats 0", spew.Sdump(seeding.Stats()))
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				stat := seeding.Stats()
				if stat.HalfOpenPeers > 0 {
					return
				}
				collect.Errorf("waiting for peers to be attempted")
			}, 5*time.Second, time.Millisecond)

			c, err := l.Accept()
			require.NoError(t, err)
			require.NoError(t, c.Close())

			// log.Println("stats 1", spew.Sdump(seeding.Stats()))

			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				stat := seeding.Stats()
				if stat.HalfOpenPeers == 0 && stat.TotalPeers == 1 {
					return
				}
				collect.Errorf("waiting for peers attempt to fail")
			}, 5*time.Second, 25*time.Millisecond)
		}
	})
}
