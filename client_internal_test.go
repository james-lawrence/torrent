package torrent

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/internal/utpx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestingConfig(t testing.TB, dir string, options ...ClientConfigOption) *ClientConfig {
	return NewDefaultClientConfig(
		metadatafilestore{root: dir},
		storage.NewFile(dir),
		ClientConfigStorageDir(dir),
		ClientConfigCacheDirectory(dir),
		ClientConfigPeerID(krpc.RandomID().String()),
		ClientConfigPortForward(false),
		// ClientConfigInfoLogger(log.New(os.Stderr, "[info] ", log.Flags())),
		// ClientConfigDebugLogger(log.New(os.Stderr, "[debug] ", log.Flags())),
		ClientConfigCompose(options...),
	)
}

func Autosocket(t *testing.T) Binder {
	var (
		bindings []sockets.Socket
	)

	s, err := utpx.New("udp", "localhost:")
	require.NoError(t, err)
	bindings = append(bindings, sockets.New(s, s))
	if addr, ok := s.Addr().(*net.UDPAddr); ok {
		s, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", addr.Port))
		require.NoError(t, err)
		bindings = append(bindings, sockets.New(s, &net.Dialer{}))
	}

	return NewSocketsBind(bindings...)
}

func totalConns(tts []Torrent) (ret int) {
	for _, tt := range tts {
		ret += tt.Stats().ActivePeers
	}
	return
}

// DeprecatedExtractClient - used to extract the underlying client.
func DeprecatedExtractClient(t Torrent) *Client {
	return t.(*torrent).cln
}

func TestTorrentInitialState(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()
	dir := t.TempDir()
	mi := testutil.GreetingTestTorrent(dir)

	cl, err := NewClient(TestingConfig(t, dir))
	require.NoError(t, err)

	tt, err := New(
		mi.HashInfoBytes(),
		OptionStorage(storage.NewFile(t.TempDir())),
		OptionChunk(2),
		OptionInfo(mi.InfoBytes),
	)
	require.NoError(t, err)
	tor := newTorrent(cl, tt)

	require.Equal(t, uint64(3), tor.chunks.pieces)

	require.NoError(t, Verify(ctx, tor))
	require.Equal(t, uint64(3), tor.chunks.pieces)

	stats := tor.Stats()

	require.Equal(t, 8, stats.Missing)
	require.True(t, tor.chunks.ChunksMissing(0))
	assert.EqualValues(t, chunkSpec{4, 1}, chunkIndexSpec(2, tor.pieceLength(0), btprotocol.Integer(tor.md.ChunkSize)))
}

func TestReducedDialTimeout(t *testing.T) {
	rdir := t.TempDir()
	cfg := NewDefaultClientConfig(
		metadatafilestore{root: rdir},
		storage.NewFile(rdir),
		ClientConfigBootstrapGlobal,
	)
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

type TestDownloadCancelParams struct {
	LeecherStorageCapacity int64
	Cancel                 bool
}

func DownloadCancelTest(t *testing.T, sb Binder, lb Binder, ps TestDownloadCancelParams) {
	ctx, _done := testx.Context(t)
	defer _done()

	greetingTempDir := t.TempDir()
	mi := testutil.GreetingTestTorrent(greetingTempDir)

	cfg := TestingConfig(
		t,
		greetingTempDir,
		ClientConfigSeed(true),
	)
	seeder, err := sb.Bind(NewClient(cfg))
	require.NoError(t, err)
	defer seeder.Close()

	tt, err := NewFromMetaInfo(mi)
	require.NoError(t, err)
	seederTorrent, _, err := seeder.Start(tt)
	require.NoError(t, err)
	require.NoError(t, Verify(ctx, seederTorrent))

	leecherDataDir := t.TempDir()
	defer os.RemoveAll(leecherDataDir)

	lcfg := TestingConfig(
		t,
		leecherDataDir,
	)
	leecher, err := lb.Bind(NewClient(lcfg))
	require.NoError(t, err)
	defer leecher.Close()

	t2, err := NewFromMetaInfo(mi, OptionChunk(2), OptionStorage(storage.NewFile(leecherDataDir)))
	require.NoError(t, err)
	leecherGreeting, added, err := leecher.Start(t2)
	require.NoError(t, err)
	assert.True(t, added)

	// if ps.Cancel {
	// 	go func() {
	// 		time.Sleep(5 * time.Millisecond)
	// 		require.NoError(t, leecher.Stop(t2))
	// 	}()
	// }

	_, err = DownloadInto(ctx, io.Discard, leecherGreeting, TuneClientPeer(seeder))
	require.NoError(t, err)
	require.Equal(t, false, leecherGreeting.(*torrent).chunks.ChunksMissing(0))
}

// Ensure that it's an error for a peer to send an invalid have message.
func TestPeerInvalidHave(t *testing.T) {
	cl, err := Autosocket(t).Bind(NewClient(TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()
	info := &metainfo.Info{
		PieceLength: 1,
		Pieces:      make([]byte, 20),
		Files:       []metainfo.FileInfo{{Length: 1}},
	}

	ts, err := NewFromInfo(info, OptionStorage(testutil.NewBadStorage()))
	require.NoError(t, err)
	tt, _added, err := cl.Start(ts)
	require.NoError(t, err)
	assert.True(t, _added)
	defer cl.Stop(ts)
	cn := newConnection(cl.config, nil, true, netip.AddrPort{}, &cl.config.extensionbits, cl.LocalPort16(), &cl.dynamicaddr)
	cn.t = tt.(*torrent)
	assert.NoError(t, cn.peerSentHave(0))
	assert.Error(t, cn.peerSentHave(1))
}

// Check that when the listen port is 0, all the protocols listened on have
// the same port, and it isn't zero.
func TestClientDynamicListenPortAllProtocols(t *testing.T) {
	cl, err := Autosocket(t).Bind(NewClient(TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()
	port := cl.LocalPort()
	assert.NotEqual(t, 0, port)
	cl.eachListener(func(s sockets.Socket) bool {
		assert.Equal(t, uint16(port), errorsx.Must(netx.AddrPort(s.Addr())).Port())
		return true
	})
}

func TestSetMaxEstablishedConn(t *testing.T) {
	var tts []Torrent
	mi := testutil.GreetingMetaInfo()
	for range 3 {
		cfg := TestingConfig(t, t.TempDir(), ClientConfigSeed(true))
		cfg.dropDuplicatePeerIds = true
		cfg.Handshaker = connections.NewHandshaker(
			connections.NewFirewall(),
		)
		cl, err := Autosocket(t).Bind(NewClient(cfg))
		require.NoError(t, err)
		defer cl.Close()
		ts, err := NewFromMetaInfo(mi)
		require.NoError(t, err)
		tt, _, err := cl.Start(ts)
		require.NoError(t, err)
		require.NoError(t, tt.Tune(TuneMaxConnections(2)))
		tts = append(tts, tt)
	}

	addPeers := func() {
		for _, tt := range tts {
			for _, _tt := range tts {
				tt.Tune(TuneClientPeer(DeprecatedExtractClient(_tt)))
			}
		}
	}
	waitTotalConns := func(num int) {
		for tot := totalConns(tts); tot != num; tot = totalConns(tts) {
			addPeers()
			log.Println("want", num, "have", tot)
			time.Sleep(time.Millisecond)
		}
	}

	addPeers()
	waitTotalConns(6)
	tts[0].Tune(TuneMaxConnections(1))
	waitTotalConns(4)
	tts[0].Tune(TuneMaxConnections(0))
	waitTotalConns(2)
	tts[0].Tune(TuneMaxConnections(1))
	addPeers()
	waitTotalConns(4)
	tts[0].Tune(TuneMaxConnections(2))
	addPeers()
	waitTotalConns(6)
}

func TestAddMetainfoWithNodes(t *testing.T) {
	// ctx, done := testx.Context(t)
	// defer done()

	cfg := TestingConfig(t, t.TempDir(), ClientConfigSeed(true), ClientConfigDebugLogger(log.Default()), ClientConfigBootstrapNone)
	// For now, we want to just jam the nodes into the table, without
	// verifying them first. Also the DHT code doesn't support mixing secure
	// and insecure nodes if security is enabled (yet).
	// cfg.DHTConfig.NoSecurity = true
	cl, err := Autosocket(t).Bind(NewClient(cfg))
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
	assert.Len(t, tt.Metadata().Trackers, 10)
	// There are 6 nodes in the torrent file.
	for sum() != int64(6*len(cl.dhtServers)) {
		time.Sleep(time.Millisecond)
	}
}
