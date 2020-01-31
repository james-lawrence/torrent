package torrent

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/missinggo/pubsub"
	"github.com/bradfitz/iter"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/storage"
)

// Ensure that no race exists between sending a bitfield, and a subsequent
// Have that would potentially alter it.
func TestSendBitfieldThenHave(t *testing.T) {
	t.SkipNow()
	cl := Client{
		config: TestingConfig(),
	}
	ts, err := New(metainfo.Hash{})
	require.NoError(t, err)
	tt := cl.newTorrent(ts)
	tt.setInfo(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*3),
		Length:      24 * (1 << 10),
		PieceLength: 8 * (1 << 10),
	})
	c := cl.newConnection(nil, false, IpPort{})
	c.setTorrent(tt)

	r, w := io.Pipe()
	c.r = r
	c.w = w
	go c.writer(time.Minute)
	c.cmu().Lock()
	c.t.piecesM.completed.Add(1)
	c.PostBitfield( /*[]bool{false, true, false}*/ )
	c.cmu().Unlock()
	c.cmu().Lock()
	c.Have(2)
	c.cmu().Unlock()
	b := make([]byte, 15)
	n, err := io.ReadFull(r, b)
	c.cmu().Lock()
	// This will cause connection.writer to terminate.
	c.closed.Set()
	c.cmu().Unlock()
	require.NoError(t, err)
	require.EqualValues(t, 15, n)
	// Here we see that the bitfield doesn't have piece 2 set, as that should
	// arrive in the following Have message.
	require.EqualValues(t, "\x00\x00\x00\x02\x05@\x00\x00\x00\x05\x04\x00\x00\x00\x02", string(b))
}

type torrentStorage struct {
	writeSem sync.Mutex
}

func (me *torrentStorage) Close() error { return nil }

func (me *torrentStorage) Piece(mp metainfo.Piece) storage.PieceImpl {
	return me
}

func (me *torrentStorage) Completion() storage.Completion {
	return storage.Completion{}
}

func (me *torrentStorage) MarkComplete() error {
	return nil
}

func (me *torrentStorage) MarkNotComplete() error {
	return nil
}

func (me *torrentStorage) ReadAt([]byte, int64) (int, error) {
	panic("shouldn't be called")
}

func (me *torrentStorage) WriteAt(b []byte, _ int64) (int, error) {
	if len(b) != defaultChunkSize {
		panic(len(b))
	}
	me.writeSem.Unlock()
	return len(b), nil
}

func BenchmarkConnectionMainReadLoop(b *testing.B) {
	cl := &Client{
		config: &ClientConfig{
			DownloadRateLimiter: unlimited,
		},
	}

	ts := &torrentStorage{}
	t := &torrent{
		cln:               cl,
		storage:           &storage.Torrent{TorrentImpl: ts},
		pieceStateChanges: pubsub.NewPubSub(),
	}
	require.NoError(b, t.setInfo(&metainfo.Info{
		Pieces:      make([]byte, 20),
		Length:      1 << 20,
		PieceLength: 1 << 20,
	}))
	t.setChunkSize(defaultChunkSize)
	t.makePieces()
	t.piecesM.ChunksPend(0)
	r, w := net.Pipe()
	cn := cl.newConnection(r, true, IpPort{})
	cn.setTorrent(t)
	mrlErr := make(chan error)
	msg := pp.Message{
		Type:  pp.Piece,
		Piece: make([]byte, defaultChunkSize),
	}
	go func() {
		cl.lock()
		err := cn.mainReadLoop()
		if err != nil {
			mrlErr <- err
		}
		close(mrlErr)
	}()
	wb := msg.MustMarshalBinary()
	b.SetBytes(int64(len(msg.Piece)))
	go func() {
		defer w.Close()
		ts.writeSem.Lock()
		for range iter.N(b.N) {
			cl.lock()
			// The chunk must be written to storage everytime, to ensure the
			// writeSem is unlocked.
			t.pieces[0].dirtyChunks.Clear()
			// TODO: figure out what this was doing....
			// cn.validReceiveChunks = map[request]struct{}{newRequestFromMessage(&msg): {}}
			cl.unlock()
			n, err := w.Write(wb)
			require.NoError(b, err)
			require.EqualValues(b, len(wb), n)
			ts.writeSem.Lock()
		}
	}()
	require.NoError(b, <-mrlErr)
	require.EqualValues(b, b.N, cn.stats.ChunksReadUseful.Int64())
}
