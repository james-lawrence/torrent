package torrent

import (
	"maps"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/internal/errorsx"
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

// TestWriterStateRerequestsChunksReturnedToMissing proves a connection picks
// work back up when chunks land in missing again after it has already
// requested them.
//
// requestable used to be drained as requests went out - each chunk removed
// from it once requested - and the refresh that would refill it is gated
// behind refreshrequestable, which updaterequestable itself parks at Inf. The
// only things that move it back are peer events. So nothing about the local
// chunk pool regaining work was visible to a connection that had drained its
// set, and it idled forever with chunks it could serve sitting in missing.
//
// the branch meant to catch exactly this (chunks.Pop returning empty while
// missing/outstanding is non-zero, which reschedules the refresh) could not
// fire on its own either: genrequests returns early on available.IsEmpty() &&
// unmodified, and available was the same bitmap as requestable, so by the time
// that branch's requestable.IsEmpty() would hold, the early return had already
// taken.
//
// a failed piece digest is the plainest way to reach this state - the chunks
// arrived, so nothing is outstanding to the peer any more, and then the whole
// piece lands back in missing with no peer event anywhere.
func TestWriterStateRerequestsChunksReturnedToMissing(t *testing.T) {
	ws := newTestWriterState(t)

	// the initialization connwriterinit performs on a live writer, which
	// newWriterState leaves out.
	ws.requestable = roaring.New()
	ws.lowrequestwatermark = max(1, int(ws.PeerMaxRequests.Load()/4))
	ws.chokeduntil = time.Now().Add(-time.Minute)

	ws.mutate(func(ws *writerstate) { ws.PeerChoked = false })

	// a torrent that wants all of its data, as TuneAutoDownload leaves it.
	ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

	ws.cmu().Lock()
	ws.claimed.AddRange(0, uint64(ws.t.chunks.cmaximum))
	ws.cmu().Unlock()

	// what a peer's opening bitfield would do: mark our view of what it has as
	// stale so the first pass actually computes a requestable set.
	ws.peerPiecesChanged()

	var requested []request
	mw := messageWriter(func(m pp.Message) error {
		if m.Type == pp.Request {
			requested = append(requested, newRequestFromMessage(&m))
		}
		return nil
	})

	gen := _connwriterRequests{writerstate: ws}
	gen.genrequests(gen.determineInterest(mw), mw)

	require.Len(t, requested, int(ws.t.chunks.cmaximum), "every chunk the peer has should have been requested")
	require.EqualValues(t, ws.t.chunks.cmaximum, ws.requestsLen(), "every requested chunk is now in flight to the peer")

	// a second pass while they're genuinely in flight must not ask again -
	// that's what requested is for, and it's the reason the drain existed.
	requested = nil
	gen.genrequests(gen.determineInterest(mw), mw)
	require.Empty(t, requested, "chunks already in flight to this peer must not be re-requested")

	// every chunk arrives, so nothing is outstanding to the peer any more.
	for _, req := range slices.Collect(maps.Values(ws.t.chunks.Outstanding())) {
		ws.mutate(func(ws *writerstate) { ws.clearRequestsLocked(req) })
		require.NoError(t, ws.t.chunks.Verify(req))
	}
	require.Zero(t, ws.requestsLen())

	// and then the pieces fail their digest, landing in failed.
	for pid := range ws.t.chunks.pieces {
		ws.t.chunks.Hashed(pid, errorsx.New("digest mismatch"))
	}
	require.EqualValues(t, ws.t.chunks.cmaximum, ws.t.chunks.Cardinality(ws.t.chunks.failed))
	require.Zero(t, ws.t.chunks.Cardinality(ws.t.chunks.missing))

	// the first pass finds nothing poppable and merges failed back into
	// missing, the second acts on it. both have to happen without a peer
	// event - that is the whole point.
	requested = nil
	gen.genrequests(gen.determineInterest(mw), mw)
	require.EqualValues(t, ws.t.chunks.cmaximum, ws.t.chunks.Cardinality(ws.t.chunks.missing), "failed chunks were never returned to missing")

	gen.genrequests(gen.determineInterest(mw), mw)
	require.NotEmpty(t, requested, "chunks returned to missing were never re-requested - the connection is stranded with work it can do")
}
