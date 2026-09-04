package torrent

import (
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/metainfo"
)

func newTestWriterState(t *testing.T) *writerstate {
	cfg := TestingConfig(t, t.TempDir(), ClientConfigSeed(true))
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	ts, err := New(metainfo.Hash{})
	require.NoError(t, err)
	tt := newTorrent(cl, ts)
	require.NoError(t, tt.setInfo(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*3),
		Length:      24 * (1 << 10),
		PieceLength: 8 * (1 << 10),
	}))

	c := cl.newConnection(nil, false, netip.AddrPort{})
	c.setTorrent(tt)

	return newWriterState(c)
}

// TestWriterStateMutateAppliesSynchronously proves ws.mutate applies its op
// immediately under lock - there is no separate drain step for mainReadLoop
// to rely on. An earlier design queued ops onto a channel that the writer
// goroutine drained on its own schedule, woken by a bare Broadcast call; that
// is a classic missed-wakeup hazard (a Broadcast fired while nothing is
// currently inside Wait() is silently lost), which intermittently stranded
// queued mutations forever and hung transfers. mutate must never have that
// failure mode: the op either has already happened by the time mutate
// returns, full stop.
func TestWriterStateMutateAppliesSynchronously(t *testing.T) {
	ws := newTestWriterState(t)

	ws.mutate(func(ws *writerstate) { ws.PeerChoked = false })

	require.False(t, ws.PeerChoked, "mutate must apply its op before returning, not queue it for later")
}

// TestWriterStateMutateConcurrentWithView proves mutate/view are actually
// safe for concurrent use (the reason they hold a lock at all) - the race
// detector, run via `go test -race`, is what actually verifies this; the
// assertions below just confirm every update was observed.
func TestWriterStateMutateConcurrentWithView(t *testing.T) {
	ws := newTestWriterState(t)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws.mutate(func(ws *writerstate) { ws.touched.AddInt(i) })
		}(i)
	}
	wg.Wait()

	count := ws.view(func(ws *writerstate) int { return int(ws.touched.GetCardinality()) })
	require.Equal(t, n, count, "every concurrent mutate must be observed - none silently lost")
}

// blockingWriter lets a test pause a Write call after it has received the
// argument slice but before it reads any bytes from it - the exact window in
// which a concurrent mutation of an aliased buffer would be observed.
type blockingWriter struct {
	writing chan struct{}
	release chan struct{}
	got     []byte
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		writing: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	close(w.writing)
	<-w.release
	w.got = append([]byte(nil), p...)
	return len(p), nil
}

// TestWriterStateFlushDoesNotAliasCurrentBuffer proves Flush hands
// FlushBuffer bytes that remain stable even if Write appends new data (e.g.
// from mainReadLoop or the writer's own cycle, as happens in production)
// before the underlying io.Writer has actually read them.
// bytes.Buffer.Bytes() returns a view into the buffer's live backing array,
// and Reset() does not discard that array - so a naive Flush that reads
// Bytes(), Resets, then writes the slice outside the lock hands out a slice
// that a subsequent Write can silently overwrite mid-flight, corrupting
// whatever is actually sent on the wire.
func TestWriterStateFlushDoesNotAliasCurrentBuffer(t *testing.T) {
	ws := newTestWriterState(t)

	bw := newBlockingWriter()
	ws.w = bw

	original := pp.NewHavePiece(1)
	encoded, err := original.MarshalBinary()
	require.NoError(t, err)

	_, err = ws.Post(original)
	require.NoError(t, err)

	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		_, ferr := ws.Flush()
		require.NoError(t, ferr)
	}()

	<-bw.writing

	// Same-length message (Have is always 9 bytes) so it reuses the exact
	// same backing array capacity Reset() left behind.
	_, err = ws.Post(pp.NewHavePiece(2))
	require.NoError(t, err)

	close(bw.release)
	<-flushed

	require.Equal(t, encoded, bw.got, "Flush must not hand out a buffer slice that a concurrent Post can still mutate before the write actually happens")
}
