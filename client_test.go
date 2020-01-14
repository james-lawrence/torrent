package torrent

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bradfitz/iter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/anacrolix/dht/v2"
	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/filecache"

	// "github.com/anacrolix/log"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestingConfig() *ClientConfig {
	cfg := NewDefaultClientConfig()
	cfg.ListenHost = LoopbackListenHost
	cfg.NoDHT = true
	cfg.DataDir = tempDir()
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableAcceptRateLimiting = true
	cfg.ListenPort = 0
	// cfg.Debug = true
	// cfg.Logger = cfg.Logger.WithText(func(m log.Msg) string {
	// 	t := m.Text()
	// 	m.Values(func(i interface{}) bool {
	// 		t += fmt.Sprintf("\n%[1]T: %[1]v", i)
	// 		return true
	// 	})
	// 	return t
	// })
	return cfg
}

func TestClientDefault(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	cl.Close()
}

func TestClientNilConfig(t *testing.T) {
	cl, err := NewClient(nil)
	require.NoError(t, err)
	cl.Close()
}

func TestBoltPieceCompletionClosedWhenClientClosed(t *testing.T) {
	cfg := TestingConfig()
	pc, err := storage.NewBoltPieceCompletion(cfg.DataDir)
	require.NoError(t, err)
	ci := storage.NewFileWithCompletion(cfg.DataDir, pc)
	defer ci.Close()
	cfg.DefaultStorage = ci
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	cl.Close()
	// And again, https://github.com/anacrolix/torrent/issues/158
	cl, err = NewClient(cfg)
	require.NoError(t, err)
	cl.Close()
}

func TestAddDropTorrent(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	dir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(dir)
	tt, added, err := cl.MaybeStart(NewFromMetaInfo(mi))
	require.NoError(t, err)
	assert.True(t, added)

	require.NoError(t, tt.Tune(TuneMaxConnections(0)))
	require.NoError(t, tt.Tune(TuneMaxConnections(1)))
	cl.Stop(tt.Metadata())
}

func TestPieceHashSize(t *testing.T) {
	assert.Equal(t, 20, pieceHash.Size())
}

func TestTorrentInitialState(t *testing.T) {
	dir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(dir)
	cl := &Client{
		config: TestingConfig(),
	}
	cl.initLogger()
	tt, err := New(
		mi.HashInfoBytes(),
		OptionStorage(storage.NewFileWithCompletion(tempDir(), storage.NewMapPieceCompletion())),
		OptionChunk(2),
	)
	require.NoError(t, err)
	tor := cl.newTorrent(tt)

	tor.cl.lock()
	err = tor.setInfoBytes(mi.InfoBytes)
	tor.cl.unlock()
	require.NoError(t, err)
	require.Len(t, tor.pieces, 3)
	tor.pendAllChunkSpecs(0)
	tor.cl.lock()
	assert.EqualValues(t, 3, int(tor.pieceNumPendingChunks(0)))
	tor.cl.unlock()
	assert.EqualValues(t, chunkSpec{4, 1}, chunkIndexSpec(2, tor.pieceLength(0), tor.chunkSize))
}

func TestReducedDialTimeout(t *testing.T) {
	cfg := NewDefaultClientConfig()
	for _, _case := range []struct {
		Max             time.Duration
		HalfOpenLimit   int
		PendingPeers    int
		ExpectedReduced time.Duration
	}{
		{cfg.NominalDialTimeout, 40, 0, cfg.NominalDialTimeout},
		{cfg.NominalDialTimeout, 40, 1, cfg.NominalDialTimeout},
		{cfg.NominalDialTimeout, 40, 39, cfg.NominalDialTimeout},
		{cfg.NominalDialTimeout, 40, 40, cfg.NominalDialTimeout / 2},
		{cfg.NominalDialTimeout, 40, 80, cfg.NominalDialTimeout / 3},
		{cfg.NominalDialTimeout, 40, 4000, cfg.NominalDialTimeout / 101},
	} {
		reduced := reducedDialTimeout(cfg.MinDialTimeout, _case.Max, _case.HalfOpenLimit, _case.PendingPeers)
		expected := _case.ExpectedReduced
		if expected < cfg.MinDialTimeout {
			expected = cfg.MinDialTimeout
		}
		if reduced != expected {
			t.Fatalf("expected %s, got %s", _case.ExpectedReduced, reduced)
		}
	}
}

func TestAddDropManyTorrents(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	for i := range iter.N(1000) {
		var spec Metadata
		binary.PutVarint((&spec).InfoHash[:], int64(i))
		_, added, err := cl.Start(spec)
		assert.NoError(t, err)
		assert.True(t, added)
		defer cl.Stop(spec)
	}
}

type FileCacheClientStorageFactoryParams struct {
	Capacity    int64
	SetCapacity bool
	Wrapper     func(*filecache.Cache) storage.ClientImpl
}

func NewFileCacheClientStorageFactory(ps FileCacheClientStorageFactoryParams) storageFactory {
	return func(dataDir string) storage.ClientImpl {
		fc, err := filecache.NewCache(dataDir)
		if err != nil {
			panic(err)
		}
		if ps.SetCapacity {
			fc.SetCapacity(ps.Capacity)
		}
		return ps.Wrapper(fc)
	}
}

type storageFactory func(string) storage.ClientImpl

func TestClientTransferDefault(t *testing.T) {
	testClientTransfer(t, testClientTransferParams{
		ExportClientStatus: true,
		LeecherStorage: NewFileCacheClientStorageFactory(FileCacheClientStorageFactoryParams{
			Wrapper: fileCachePieceResourceStorage,
		}),
	})
}

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

func fileCachePieceResourceStorage(fc *filecache.Cache) storage.ClientImpl {
	return storage.NewResourcePieces(fc.AsResourceProvider())
}

func testClientTransferSmallCache(t *testing.T, setReadahead bool, readahead int64) {
	testClientTransfer(t, testClientTransferParams{
		LeecherStorage: NewFileCacheClientStorageFactory(FileCacheClientStorageFactoryParams{
			SetCapacity: true,
			// Going below the piece length means it can't complete a piece so
			// that it can be hashed.
			Capacity: 5,
			Wrapper:  fileCachePieceResourceStorage,
		}),
		SetReadahead: setReadahead,
		// Can't readahead too far or the cache will thrash and drop data we
		// thought we had.
		Readahead:          readahead,
		ExportClientStatus: true,
	})
}

func TestClientTransferSmallCachePieceSizedReadahead(t *testing.T) {
	testClientTransferSmallCache(t, true, 5)
}

func TestClientTransferSmallCacheLargeReadahead(t *testing.T) {
	testClientTransferSmallCache(t, true, 15)
}

func TestClientTransferSmallCacheDefaultReadahead(t *testing.T) {
	testClientTransferSmallCache(t, false, -1)
}

func TestClientTransferVarious(t *testing.T) {
	count := 0
	// Leecher storage
	for _, ls := range []storageFactory{
		NewFileCacheClientStorageFactory(FileCacheClientStorageFactoryParams{
			Wrapper: fileCachePieceResourceStorage,
		}),
		storage.NewBoltDB,
	} {
		// Seeder storage
		for _, ss := range []func(string) storage.ClientImpl{
			storage.NewFile,
			storage.NewMMap,
		} {
			for _, responsive := range []bool{false, true} {
				testClientTransfer(t, testClientTransferParams{
					Responsive:     responsive,
					SeederStorage:  ss,
					LeecherStorage: ls,
				})
				for _, readahead := range []int64{-1, 0, 1, 2, 3, 4, 5, 6, 9, 10, 11, 12, 13, 14, 15, 20} {
					count++
					log.Println("running test", count)
					testClientTransfer(t, testClientTransferParams{
						SeederStorage:  ss,
						Responsive:     responsive,
						SetReadahead:   true,
						Readahead:      readahead,
						LeecherStorage: ls,
					})
				}
			}
		}
	}
}

type testClientTransferParams struct {
	Responsive                 bool
	Readahead                  int64
	SetReadahead               bool
	ExportClientStatus         bool
	LeecherStorage             func(string) storage.ClientImpl
	SeederStorage              func(string) storage.ClientImpl
	SeederUploadRateLimiter    *rate.Limiter
	LeecherDownloadRateLimiter *rate.Limiter
}

func logPieceStateChanges(t *torrent) {
	sub := t.SubscribePieceStateChanges()
	go func() {
		defer sub.Close()
		for e := range sub.Values {
			log.Printf("%p %#v", t, e)
		}
	}()
}

// Creates a seeder and a leecher, and ensures the data transfers when a read
// is attempted on the leecher.
func testClientTransfer(t *testing.T, ps testClientTransferParams) {
	greetingTempDir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(greetingTempDir)
	// Create seeder and a Torrent.
	cfg := TestingConfig()
	cfg.Seed = true
	if ps.SeederUploadRateLimiter != nil {
		cfg.UploadRateLimiter = ps.SeederUploadRateLimiter
	}
	// cfg.ListenAddr = "localhost:4000"
	if ps.SeederStorage != nil {
		cfg.DefaultStorage = ps.SeederStorage(greetingTempDir)
		defer cfg.DefaultStorage.Close()
	} else {
		cfg.DataDir = greetingTempDir
	}
	seeder, err := NewClient(cfg)
	require.NoError(t, err)
	if ps.ExportClientStatus {
		defer testutil.ExportStatusWriter(seeder, "s")()
	}

	seederTorrent, _, err := seeder.MaybeStart(NewFromMetaInfo(mi))
	require.NoError(t, err)
	// Run a Stats right after Closing the Client. This will trigger the Stats
	// panic in #214 caused by RemoteAddr on Closed uTP sockets.
	defer seederTorrent.Stats()
	defer seeder.Close()
	seederTorrent.VerifyData()

	// TODO REMOVE ME.
	require.True(t, seeder.WaitAll())
	// log.Println("SEEDER MISSING PIECES", seederTorrent.(*torrent).piecesM.Missing())
	// log.Printf("\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n")

	// Create leecher and a Torrent.
	leecherDataDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(leecherDataDir)
	cfg = TestingConfig()
	if ps.LeecherStorage == nil {
		cfg.DataDir = leecherDataDir
	} else {
		cfg.DefaultStorage = ps.LeecherStorage(leecherDataDir)
	}
	if ps.LeecherDownloadRateLimiter != nil {
		cfg.DownloadRateLimiter = ps.LeecherDownloadRateLimiter
	}
	cfg.Seed = false
	//cfg.Debug = true
	leecher, err := NewClient(cfg)
	require.NoError(t, err)
	defer leecher.Close()
	if ps.ExportClientStatus {
		defer testutil.ExportStatusWriter(leecher, "l")()
	}
	leecherTorrent, added, err := leecher.MaybeStart(
		NewFromMetaInfo(
			mi,
			OptionChunk(2),
		),
	)
	require.NoError(t, err)
	assert.True(t, added)

	// This was used when observing coalescing of piece state changes.
	//logPieceStateChanges(leecherTorrent)

	// Now do some things with leecher and seeder.
	leecherTorrent.AddClientPeer(seeder)
	require.EqualValues(t, 1, leecherTorrent.Stats().PendingPeers)
	// The Torrent should not be interested in obtaining peers, so the one we
	// just added should be the only one.
	assert.False(t, leecherTorrent.Seeding())
	r := leecherTorrent.NewReader()
	defer r.Close()
	if ps.Responsive {
		r.SetResponsive()
	}
	if ps.SetReadahead {
		r.SetReadahead(ps.Readahead)
	}
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
	_greeting, err := ioutil.ReadAll(r)
	assert.NoError(t, err)
	assert.EqualValues(t, testutil.GreetingFileContents, _greeting)
}

// Check that after completing leeching, a leecher transitions to a seeding
// correctly. Connected in a chain like so: Seeder <-> Leecher <-> LeecherLeecher.
func TestSeedAfterDownloading(t *testing.T) {
	greetingTempDir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(greetingTempDir)

	cfg := TestingConfig()
	cfg.Seed = true
	cfg.DataDir = greetingTempDir
	seeder, err := NewClient(cfg)
	require.NoError(t, err)
	defer seeder.Close()
	defer testutil.ExportStatusWriter(seeder, "s")()

	seederTorrent, ok, err := seeder.MaybeStart(NewFromMetaInfo(mi))
	require.NoError(t, err)
	assert.True(t, ok)
	seederTorrent.VerifyData()
	require.True(t, seeder.WaitAll())
	log.Printf("SEEDER %p c(%p)\n", seederTorrent, seederTorrent.(*torrent).piecesM)

	cfg = TestingConfig()
	cfg.Seed = true
	cfg.DataDir, err = ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(cfg.DataDir)
	leecher, err := NewClient(cfg)
	require.NoError(t, err)
	defer leecher.Close()
	defer testutil.ExportStatusWriter(leecher, "l")()

	cfg = TestingConfig()
	cfg.Seed = false
	cfg.DataDir, err = ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(cfg.DataDir)
	leecherLeecher, _ := NewClient(cfg)
	require.NoError(t, err)
	defer leecherLeecher.Close()
	defer testutil.ExportStatusWriter(leecherLeecher, "ll")()
	leecherGreeting, ok, err := leecher.MaybeStart(NewFromMetaInfo(mi, OptionChunk(2)))
	require.NoError(t, err)
	assert.True(t, ok)
	log.Printf("LEECHER %p c(%p)\n", leecherGreeting, leecherGreeting.(*torrent).piecesM)

	llg, ok, err := leecherLeecher.MaybeStart(NewFromMetaInfo(mi, OptionChunk(3)))
	require.NoError(t, err)
	assert.True(t, ok)
	log.Printf("LEECHER2 %p c(%p)\n", llg, llg.(*torrent).piecesM)

	// Simultaneously DownloadAll in Leecher, and read the contents
	// consecutively in LeecherLeecher. This non-deterministically triggered a
	// case where the leecher wouldn't unchoke the LeecherLeecher.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := llg.NewReader()
		defer r.Close()
		b, err := ioutil.ReadAll(r)
		require.NoError(t, err)
		assert.EqualValues(t, testutil.GreetingFileContents, b)
	}()
	done := make(chan struct{})
	defer close(done)
	go leecherGreeting.AddClientPeer(seeder)
	go leecherGreeting.AddClientPeer(leecherLeecher)
	wg.Add(1)
	go func() {
		defer wg.Done()
		leecherGreeting.DownloadAll()
		leecher.WaitAll()
	}()
	wg.Wait()
}

func TestMergingTrackersByAddingSpecs(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	spec := Metadata{}
	T, added, _ := cl.Start(spec)
	if !added {
		t.FailNow()
	}
	spec.Trackers = [][]string{{"http://a"}, {"udp://b"}}
	_, added, _ = cl.Start(spec)
	assert.False(t, added)
	if tor, ok := T.(*torrent); ok {
		assert.EqualValues(t, [][]string{{"http://a"}, {"udp://b"}}, tor.metainfo.AnnounceList)
		// Because trackers are disabled in TestingConfig.
		assert.EqualValues(t, 0, len(tor.trackerAnnouncers))
	} else {
		t.FailNow()
	}
}

// We read from a piece which is marked completed, but is missing data.
func TestCompletedPieceWrongSize(t *testing.T) {
	cfg := TestingConfig()
	cfg.DefaultStorage = badStorage{}
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	info := metainfo.Info{
		PieceLength: 15,
		Pieces:      make([]byte, 20),
		Files: []metainfo.FileInfo{
			{Path: []string{"greeting"}, Length: 13},
		},
	}

	b, err := bencode.Marshal(info)
	require.NoError(t, err)
	ts, err := New(metainfo.HashBytes(b), OptionInfo(b))
	require.NoError(t, err)
	tt, new, err := cl.Start(ts)
	require.NoError(t, err)
	defer cl.Stop(ts)
	assert.True(t, new)
	r := tt.NewReader()
	defer r.Close()
	b, err = ioutil.ReadAll(r)
	assert.Len(t, b, 13)
	assert.NoError(t, err)
}

func BenchmarkAddLargeTorrent(b *testing.B) {
	cfg := TestingConfig()
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cl, err := NewClient(cfg)
	require.NoError(b, err)
	defer cl.Close()
	b.ReportAllocs()
	for range iter.N(b.N) {
		t, err := NewFromMetaInfoFile("testdata/bootstrap.dat.torrent")
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
	seederDataDir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(seederDataDir)
	cfg := TestingConfig()
	cfg.Seed = true
	cfg.DataDir = seederDataDir
	seeder, err := NewClient(cfg)
	require.Nil(t, err)
	defer seeder.Close()
	tt, err := NewFromMetaInfo(mi)
	require.Nil(t, err)
	seederTorrent, _, _ := seeder.Start(tt)
	seederTorrent.VerifyData()
	leecherDataDir, err := ioutil.TempDir("", "")
	require.Nil(t, err)
	defer os.RemoveAll(leecherDataDir)
	cfg = TestingConfig()
	cfg.DataDir = leecherDataDir
	leecher, err := NewClient(cfg)
	require.Nil(t, err)
	defer leecher.Close()
	tt, err = NewFromMetaInfo(mi, OptionChunk(2))
	require.Nil(t, err)
	leecherTorrent, _, _ := leecher.Start(tt)
	leecherTorrent.AddClientPeer(seeder)
	reader := leecherTorrent.NewReader()
	defer reader.Close()
	reader.SetReadahead(0)
	reader.SetResponsive()
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
	seederDataDir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(seederDataDir)
	cfg := TestingConfig()
	cfg.Seed = true
	cfg.DataDir = seederDataDir
	seeder, err := NewClient(cfg)
	require.Nil(t, err)
	defer seeder.Close()
	st, err := NewFromMetaInfo(mi)
	require.Nil(t, err)
	seederTorrent, _, _ := seeder.Start(st)
	seederTorrent.VerifyData()
	leecherDataDir, err := ioutil.TempDir("", "")
	require.Nil(t, err)
	defer os.RemoveAll(leecherDataDir)
	cfg = TestingConfig()
	cfg.DataDir = leecherDataDir
	leecher, err := NewClient(cfg)
	require.Nil(t, err)
	defer leecher.Close()
	lt, err := NewFromMetaInfo(mi, OptionChunk(2))
	require.Nil(t, err)
	leecherTorrent, _, _ := leecher.Start(lt)
	leecherTorrent.AddClientPeer(seeder)
	reader := leecherTorrent.NewReader()
	defer reader.Close()
	reader.SetReadahead(0)
	reader.SetResponsive()
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
	assert.EqualError(t, err, "torrent closed")
	assert.EqualValues(t, 0, n)
}

func TestDHTInheritBlocklist(t *testing.T) {
	ipl := iplist.New(nil)
	require.NotNil(t, ipl)
	cfg := TestingConfig()
	cfg.IPBlocklist = ipl
	cfg.NoDHT = false
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	numServers := 0
	cl.eachDhtServer(func(s *dht.Server) {
		assert.Equal(t, ipl, s.IPBlocklist())
		numServers++
	})
	assert.EqualValues(t, 2, numServers)
}

// Check that stuff is merged in subsequent AddTorrentSpec for the same
// infohash.
func TestAddTorrentSpecMerging(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	dir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(dir)
	ts, err := NewFromMetaInfo(mi, OptionDisplayName("foo"))
	require.NoError(t, err)
	tt, added, err := cl.Start(ts)
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "foo", tt.(*torrent).displayName)
	_, added, err = cl.Start(ts.merge(OptionDisplayName("bar")))
	require.NoError(t, err)
	require.False(t, added)
	assert.Equal(t, "bar", tt.(*torrent).displayName)
}

func TestTorrentDroppedBeforeGotInfo(t *testing.T) {
	dir, mi := testutil.GreetingTestTorrent()
	os.RemoveAll(dir)
	cl, _ := NewClient(TestingConfig())
	defer cl.Close()
	ts, err := New(mi.HashInfoBytes())
	require.NoError(t, err)
	tt, _, _ := cl.Start(ts)
	cl.Stop(ts)
	assert.EqualValues(t, 0, len(cl.Torrents()))
	select {
	case <-tt.GotInfo():
		t.FailNow()
	default:
	}
}

func writeTorrentData(ts *storage.Torrent, info metainfo.Info, b []byte) {
	for i := range iter.N(info.NumPieces()) {
		p := info.Piece(i)
		ts.Piece(p).WriteAt(b[p.Offset():p.Offset()+p.Length()], 0)
	}
}

func testAddTorrentPriorPieceCompletion(t *testing.T, alreadyCompleted bool, csf func(*filecache.Cache) storage.ClientImpl) {
	fileCacheDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(fileCacheDir)
	fileCache, err := filecache.NewCache(fileCacheDir)
	require.NoError(t, err)
	greetingDataTempDir, greetingMetainfo := testutil.GreetingTestTorrent()
	defer os.RemoveAll(greetingDataTempDir)
	filePieceStore := csf(fileCache)
	defer filePieceStore.Close()
	info, err := greetingMetainfo.UnmarshalInfo()
	require.NoError(t, err)
	ih := greetingMetainfo.HashInfoBytes()
	greetingData, err := storage.NewClient(filePieceStore).OpenTorrent(&info, ih)
	require.NoError(t, err)
	writeTorrentData(greetingData, info, []byte(testutil.GreetingFileContents))
	// require.Equal(t, len(testutil.GreetingFileContents), written)
	// require.NoError(t, err)
	for i := 0; i < info.NumPieces(); i++ {
		p := info.Piece(i)
		if alreadyCompleted {
			require.NoError(t, greetingData.Piece(p).MarkComplete())
		}
	}
	cfg := TestingConfig()
	// TODO: Disable network option?
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.DefaultStorage = filePieceStore
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	ts, err := NewFromMetaInfo(greetingMetainfo)
	require.NoError(t, err)
	tt, _, err := cl.Start(ts)
	require.NoError(t, err)
	psrs := tt.PieceStateRuns()
	assert.Len(t, psrs, 1)
	assert.EqualValues(t, 3, psrs[0].Length)
	assert.Equal(t, alreadyCompleted, psrs[0].Complete)
	if alreadyCompleted {
		r := tt.NewReader()
		b, err := ioutil.ReadAll(r)
		assert.NoError(t, err)
		assert.EqualValues(t, testutil.GreetingFileContents, b)
	}
}

func TestAddTorrentPiecesAlreadyCompleted(t *testing.T) {
	testAddTorrentPriorPieceCompletion(t, true, fileCachePieceResourceStorage)
}

func TestAddTorrentPiecesNotAlreadyCompleted(t *testing.T) {
	testAddTorrentPriorPieceCompletion(t, false, fileCachePieceResourceStorage)
}

func TestAddMetainfoWithNodes(t *testing.T) {
	cfg := TestingConfig()
	cfg.ListenHost = func(string) string { return "" }
	cfg.NoDHT = false
	cfg.DhtStartingNodes = func() ([]dht.Addr, error) { return nil, nil }
	// For now, we want to just jam the nodes into the table, without
	// verifying them first. Also the DHT code doesn't support mixing secure
	// and insecure nodes if security is enabled (yet).
	// cfg.DHTConfig.NoSecurity = true
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	sum := func() (ret int64) {
		cl.eachDhtServer(func(s *dht.Server) {
			ret += s.Stats().OutboundQueriesAttempted
		})
		return
	}
	assert.EqualValues(t, 0, sum())
	ts, err := NewFromMetaInfoFile("metainfo/testdata/issue_65a.torrent")
	require.NoError(t, err)
	tt, _, err := cl.Start(ts)
	require.NoError(t, err)
	// Nodes are not added or exposed in Torrent's metainfo. We just randomly
	// check if the announce-list is here instead. TODO: Add nodes.
	assert.Len(t, tt.(*torrent).metainfo.AnnounceList, 5)
	// There are 6 nodes in the torrent file.
	for sum() != int64(6*len(cl.dhtServers)) {
		time.Sleep(time.Millisecond)
	}
}

type testDownloadCancelParams struct {
	SetLeecherStorageCapacity bool
	LeecherStorageCapacity    int64
	Cancel                    bool
}

func testDownloadCancel(t *testing.T, ps testDownloadCancelParams) {
	greetingTempDir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(greetingTempDir)
	cfg := TestingConfig()
	cfg.Seed = true
	cfg.DataDir = greetingTempDir
	seeder, err := NewClient(cfg)
	require.NoError(t, err)
	defer seeder.Close()
	defer testutil.ExportStatusWriter(seeder, "s")()
	tt, err := NewFromMetaInfo(mi)
	require.NoError(t, err)
	seederTorrent, _, _ := seeder.Start(tt)
	seederTorrent.VerifyData()
	leecherDataDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(leecherDataDir)
	fc, err := filecache.NewCache(leecherDataDir)
	require.NoError(t, err)
	if ps.SetLeecherStorageCapacity {
		fc.SetCapacity(ps.LeecherStorageCapacity)
	}
	cfg.DefaultStorage = storage.NewResourcePieces(fc.AsResourceProvider())
	cfg.DataDir = leecherDataDir
	leecher, err := NewClient(cfg)
	require.NoError(t, err)
	defer leecher.Close()
	defer testutil.ExportStatusWriter(leecher, "l")()
	t2, err := NewFromMetaInfo(mi, OptionChunk(2))
	require.NoError(t, err)
	leecherGreeting, added, err := leecher.Start(t2)
	require.NoError(t, err)
	assert.True(t, added)
	psc := leecherGreeting.SubscribePieceStateChanges()
	defer psc.Close()

	leecherGreeting.(*torrent).cl.lock()
	leecherGreeting.(*torrent).downloadPiecesLocked(0, leecherGreeting.(*torrent).numPieces())
	if ps.Cancel {
		leecherGreeting.(*torrent).cancelPiecesLocked(0, leecherGreeting.(*torrent).NumPieces())
	}
	leecherGreeting.(*torrent).cl.unlock()
	done := make(chan struct{})
	defer close(done)
	go leecherGreeting.AddClientPeer(seeder)
	completes := make(map[int]bool, 3)
	expected := func() map[int]bool {
		if ps.Cancel {
			return map[int]bool{0: false, 1: false, 2: false}
		}
		return map[int]bool{0: true, 1: true, 2: true}
	}()
	for !reflect.DeepEqual(completes, expected) {
		_v := <-psc.Values
		v := _v.(PieceStateChange)
		completes[v.Index] = v.Complete
	}
}

func TestTorrentDownloadAll(t *testing.T) {
	testDownloadCancel(t, testDownloadCancelParams{})
}

func TestTorrentDownloadAllThenCancel(t *testing.T) {
	testDownloadCancel(t, testDownloadCancelParams{
		Cancel: true,
	})
}

// Ensure that it's an error for a peer to send an invalid have message.
func TestPeerInvalidHave(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	info := metainfo.Info{
		PieceLength: 1,
		Pieces:      make([]byte, 20),
		Files:       []metainfo.FileInfo{{Length: 1}},
	}

	ts, err := NewFromInfo(info, OptionStorage(badStorage{}))
	require.NoError(t, err)
	tt, _added, err := cl.Start(ts)
	require.NoError(t, err)
	assert.True(t, _added)
	defer cl.Stop(ts)
	cn := &connection{
		t: tt.(*torrent),
	}
	assert.NoError(t, cn.peerSentHave(0))
	assert.Error(t, cn.peerSentHave(1))
}

func TestPieceCompletedInStorageButNotClient(t *testing.T) {
	greetingTempDir, greetingMetainfo := testutil.GreetingTestTorrent()
	defer os.RemoveAll(greetingTempDir)
	cfg := TestingConfig()
	cfg.DataDir = greetingTempDir
	seeder, err := NewClient(TestingConfig())
	require.NoError(t, err)
	ts, err := NewFromMetaInfo(greetingMetainfo)
	require.NoError(t, err)
	seeder.Start(ts)
}

// Check that when the listen port is 0, all the protocols listened on have
// the same port, and it isn't zero.
func TestClientDynamicListenPortAllProtocols(t *testing.T) {
	cl, err := NewClient(TestingConfig())
	require.NoError(t, err)
	defer cl.Close()
	port := cl.LocalPort()
	assert.NotEqual(t, 0, port)
	cl.eachListener(func(s socket) bool {
		assert.Equal(t, port, missinggo.AddrPort(s.Addr()))
		return true
	})
}

func TestClientDynamicListenTCPOnly(t *testing.T) {
	cfg := TestingConfig()
	cfg.DisableUTP = true
	cfg.DisableTCP = false
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	assert.NotEqual(t, 0, cl.LocalPort())
}

func TestClientDynamicListenUTPOnly(t *testing.T) {
	cfg := TestingConfig()
	cfg.DisableTCP = true
	cfg.DisableUTP = false
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	assert.NotEqual(t, 0, cl.LocalPort())
}

func totalConns(tts []*torrent) (ret int) {
	for _, tt := range tts {
		tt.cl.lock()
		ret += len(tt.conns)
		tt.cl.unlock()
	}
	return
}

func TestSetMaxEstablishedConn(t *testing.T) {
	var tts []*torrent
	mi := testutil.GreetingMetaInfo().HashInfoBytes()
	cfg := TestingConfig()
	cfg.DisableAcceptRateLimiting = true
	cfg.dropDuplicatePeerIds = true
	for i := range iter.N(3) {
		cl, err := NewClient(cfg)
		require.NoError(t, err)
		defer cl.Close()
		ts, err := New(mi)
		require.NoError(t, err)
		tt, _, _ := cl.Start(ts)
		require.NoError(t, tt.Tune(TuneMaxConnections(2)))
		defer testutil.ExportStatusWriter(cl, fmt.Sprintf("%d", i))()
		tts = append(tts, tt.(*torrent))
	}
	addPeers := func() {
		for _, tt := range tts {
			for _, _tt := range tts {
				// if tt != _tt {
				tt.AddClientPeer(_tt.cl)
				// }
			}
		}
	}
	waitTotalConns := func(num int) {
		for totalConns(tts) != num {
			addPeers()
			time.Sleep(time.Millisecond)
		}
	}
	addPeers()
	waitTotalConns(6)
	tts[0].SetMaxEstablishedConns(1)
	waitTotalConns(4)
	tts[0].SetMaxEstablishedConns(0)
	waitTotalConns(2)
	tts[0].SetMaxEstablishedConns(1)
	addPeers()
	waitTotalConns(4)
	tts[0].SetMaxEstablishedConns(2)
	addPeers()
	waitTotalConns(6)
}

// Creates a file containing its own name as data. Make a metainfo from that, adds it to the given
// client, and returns a magnet link.
func makeMagnet(t *testing.T, cl *Client, dir string, name string) string {
	os.MkdirAll(dir, 0770)
	file, err := os.Create(filepath.Join(dir, name))
	require.NoError(t, err)
	file.Write([]byte(name))
	file.Close()
	mi := metainfo.MetaInfo{}
	mi.SetDefaults()
	info := metainfo.Info{PieceLength: 256 * 1024}
	err = info.BuildFromFilePath(filepath.Join(dir, name))
	require.NoError(t, err)
	mi.InfoBytes, err = bencode.Marshal(info)
	require.NoError(t, err)
	magnet := mi.Magnet(name, mi.HashInfoBytes()).String()
	ts, err := NewFromMetaInfo(&mi)
	require.NoError(t, err)
	tr, _, err := cl.Start(ts)
	require.NoError(t, err)
	require.True(t, tr.Seeding())
	tr.VerifyData()
	return magnet
}

// https://github.com/anacrolix/torrent/issues/114
func TestMultipleTorrentsWithEncryption(t *testing.T) {
	testSeederLeecherPair(
		t,
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

// Test that the leecher can download a torrent in its entirety from the seeder. Note that the
// seeder config is done first.
func testSeederLeecherPair(t *testing.T, seeder func(*ClientConfig), leecher func(*ClientConfig)) {
	cfg := TestingConfig()
	cfg.Seed = true
	cfg.DataDir = filepath.Join(cfg.DataDir, "server")
	os.Mkdir(cfg.DataDir, 0755)
	seeder(cfg)
	server, err := NewClient(cfg)
	require.NoError(t, err)
	defer server.Close()
	defer testutil.ExportStatusWriter(server, "s")()
	magnet1 := makeMagnet(t, server, cfg.DataDir, "test1")
	// Extra torrents are added to test the seeder having to match incoming obfuscated headers
	// against more than one torrent. See issue #114
	makeMagnet(t, server, cfg.DataDir, "test2")
	for i := 0; i < 100; i++ {
		makeMagnet(t, server, cfg.DataDir, fmt.Sprintf("test%d", i+2))
	}
	cfg = TestingConfig()
	cfg.DataDir = filepath.Join(cfg.DataDir, "client")
	leecher(cfg)
	client, err := NewClient(cfg)
	require.NoError(t, err)
	defer client.Close()
	defer testutil.ExportStatusWriter(client, "c")()
	ts, err := NewFromMagnet(magnet1)
	require.NoError(t, err)
	tr, _, err := client.Start(ts)
	require.NoError(t, err)

	tr.AddClientPeer(server)
	<-tr.GotInfo()
	tr.DownloadAll()
	client.WaitAll()
}

// This appears to be the situation with the S3 BitTorrent client.
func TestObfuscatedHeaderFallbackSeederDisallowsLeecherPrefers(t *testing.T) {
	// Leecher prefers obfuscation, but the seeder does not allow it.
	testSeederLeecherPair(
		t,
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = false
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

func TestObfuscatedHeaderFallbackSeederRequiresLeecherPrefersNot(t *testing.T) {
	// Leecher prefers no obfuscation, but the seeder enforces it.
	testSeederLeecherPair(
		t,
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = true
			cfg.HeaderObfuscationPolicy.RequirePreferred = true
		},
		func(cfg *ClientConfig) {
			cfg.HeaderObfuscationPolicy.Preferred = false
			cfg.HeaderObfuscationPolicy.RequirePreferred = false
		},
	)
}

func TestClientAddressInUse(t *testing.T) {
	s, _ := NewUtpSocket("udp", ":50007", nil)
	if s != nil {
		defer s.Close()
	}
	cfg := TestingConfig().SetListenAddr(":50007")
	cl, err := NewClient(cfg)
	require.Error(t, err)
	require.Nil(t, cl)
}

func TestClientHasDhtServersWhenUtpDisabled(t *testing.T) {
	cc := TestingConfig()
	cc.DisableUTP = true
	cc.NoDHT = false
	cl, err := NewClient(cc)
	require.NoError(t, err)
	defer cl.Close()
	assert.NotEmpty(t, cl.DhtServers())
}

func TestIssue335(t *testing.T) {
	dir, mi := testutil.GreetingTestTorrent()
	defer os.RemoveAll(dir)
	cfg := TestingConfig()
	cfg.Seed = false
	cfg.Debug = true
	cfg.DataDir = dir
	comp, err := storage.NewBoltPieceCompletion(dir)
	require.NoError(t, err)
	defer comp.Close()
	cfg.DefaultStorage = storage.NewMMapWithCompletion(dir, comp)
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	ts, err := NewFromMetaInfo(mi)
	require.NoError(t, err)
	_, added, err := cl.Start(ts)
	require.NoError(t, err)
	assert.True(t, added)
	require.True(t, cl.WaitAll())
	require.NoError(t, cl.Stop(ts))
	_, added, err = cl.Start(ts)
	require.NoError(t, err)
	assert.True(t, added)
	require.True(t, cl.WaitAll())
}
