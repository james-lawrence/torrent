package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/bep0006"
	"github.com/james-lawrence/torrent/bep0009"
	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/cstate"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/backoffx"
	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/timex"
)

func RunHandshookConn(c *connection, t *torrent) error {
	const retrydelay = 10 * time.Second

	remotreaddr := c.conn.RemoteAddr()
	c.setTorrent(t)

	c.conn.SetWriteDeadline(time.Time{})
	c.r = deadlineReader{c.conn, c.r}
	t.lastConnection.Store(langx.Autoptr(time.Now()))
	completedHandshakeConnectionFlags.Add(c.connectionFlags(), 1)

	defer t.dropConnection(c)

	if err := t.addConnection(c); err != nil {
		return errorsx.Wrap(err, "error adding connection")
	}

	defer c.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// Constructed here, before ConnExtensions or either goroutine runs, so
	// currentbuffer (and everything else writer-owned) is reachable from the
	// very first write - the pre-spawn handshake included - and mainReadLoop
	// can safely call ws.mutate/ws.view from the moment it starts. Passed
	// down explicitly everywhere it's needed rather than stashed on
	// *connection.
	ws := newWriterState(c)

	if err := ConnExtensions(ctx, ws); err != nil {
		err = errorsx.LogErr(errorsx.Wrap(err, "error configuring connection (extensions)"))
		cancel(err)
		return err
	}

	go func() {
		err := connwriterinit(ctx, ws, 10*time.Second)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		cancel(err)
		c.Close()
	}()

	go func() {
		err := connreaderinit(ctx, c, ws, 10*time.Second)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		cancel(err)
		c.Close()
	}()

	if err := c.mainReadLoop(ctx, ws); err != nil {
		// check for errors from the writer.
		err = errorsx.Compact(context.Cause(ctx), err)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		err = errorsx.Wrapf(err, "%s - %s: error during main read loop", c.PeerClientName, remotreaddr)
		cancel(err)
		c.cfg.Handshaker.Release(c.conn, err)
		return err
	}

	return errorsx.LogErr(context.Cause(ctx))
}

// See the order given in Transmission's tr_peerMsgsNew.
func ConnExtensions(ctx context.Context, ws *writerstate) error {
	cn := ws.connection
	cn.cfg.debug().Println("conn extensions initiated")
	defer cn.cfg.debug().Println("conn extensions completed")
	return cstate.Run(ctx, connexinit(ws, connexfast(ws, connexdht(ws, connflush(ws, nil)))), cn.cfg.debug())
}

func connflush(ws *writerstate, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		_, err := ws.Flush()
		if err != nil {
			return cstate.Failure(errorsx.Wrap(err, "failed to flush requests"))
		}
		return n
	})
}

func connexinit(ws *writerstate, n cstate.T) cstate.T {
	cn := ws.connection
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		if !cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitExtended) {
			return n
		}

		dynamicport := langx.FirstNonZero(cn.connaddr.Port(), cn.localport)
		defer cn.cfg.debug().Println("extended handshake extension completed", cn.localport, cn.connaddr)

		// TODO: We can figure the port and address out specific to the socket
		// used.
		msg := btprotocol.ExtendedHandshakeMessage{
			M:            cn.cfg.extensions,
			V:            cn.cfg.extendedHandshakeClientVersion,
			Reqq:         cn.cfg.maximumOutstandingRequests,
			YourIp:       btprotocol.CompactIp(cn.remoteAddr.Addr().AsSlice()),
			Encryption:   cn.cfg.HeaderObfuscationPolicy.Preferred || cn.cfg.HeaderObfuscationPolicy.Required,
			Port:         int(dynamicport),
			MetadataSize: cn.t.metadatalen(),
			Ipv4:         btprotocol.CompactIp(netx.IP4FromAddr(cn.connaddr.Addr())),
			Ipv6:         netx.IP6FromAddr(cn.connaddr.Addr()),
		}
		// cn.cfg.debug().Printf("%s extended handshake: %s\n", cn, spew.Sdump(msg))

		encoded, err := bencode.Marshal(msg)
		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		_, err = ws.Post(btprotocol.NewExtendedHandshake(encoded))

		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		return n
	})
}

func connexfast(ws *writerstate, n cstate.T) cstate.T {
	cn := ws.connection
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		defer cn.cfg.debug().Printf("c(%p) seed(%t) fast extension completed\n", cn, cn.t.seeding())
		if !cn.supported(btprotocol.ExtensionBitFast) {
			cn.sentHaves = cn.t.chunks.CompletedBitmap()
			if _, err := ws.PostBitfield(cn.sentHaves); err != nil {
				return cstate.Failure(err)
			}
			return n
		}

		if cn.t.haveInfo() {
			cn.peerfastset = errorsx.Zero(bep0006.AllowedFastSet(cn.remoteAddr.Addr(), cn.t.md.ID, cn.t.chunks.pieces, min(32, cn.t.chunks.pieces)))
		}

		switch readable := cn.t.chunks.Readable(); readable {
		case 0:
			cn.cfg.debug().Printf("c(%p) seed(%t) posting allow fast have none: %d/%d\n", cn, cn.t.seeding(), readable, cn.t.chunks.cmaximum)
			if _, err := ws.Post(btprotocol.NewHaveNone()); err != nil {
				return cstate.Failure(err)
			}
			cn.sentHaves.Clear()
			return n
		case uint64(cn.t.chunks.cmaximum):
			cn.cfg.debug().Printf("c(%p) seed(%t) posting allow fast have all: %d/%d\n", cn, cn.t.seeding(), readable, cn.t.chunks.cmaximum)
			if _, err := ws.Post(btprotocol.NewHaveAll()); err != nil {
				return cstate.Failure(err)
			}

			cn.sentHaves.AddRange(0, cn.t.chunks.pieces)

			for _, v := range cn.peerfastset.ToArray() {
				if _, err := ws.Post(btprotocol.NewAllowedFast(v)); err != nil {
					return cstate.Failure(err)
				}
			}

			return n
		default:
			cn.cfg.debug().Printf("c(%p) seed(%t) posting bitfield: r(%d) u(%d) c(%d) cmax(%d)\n", cn, cn.t.seeding(), readable, cn.t.chunks.Cardinality(cn.t.chunks.unverified), cn.t.chunks.Cardinality(cn.t.chunks.completed), cn.t.chunks.cmaximum)
			cn.sentHaves = cn.t.chunks.CompletedBitmap()
			if _, err := ws.PostBitfield(cn.sentHaves); err != nil {
				return cstate.Failure(err)
			}
		}

		return n
	})
}

func connexdht(ws *writerstate, n cstate.T) cstate.T {
	cn := ws.connection
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		connaddr := cn.connaddr
		port := langx.DefaultIfZero(cn.t.cln.LocalPort16(), connaddr.Port())
		if !(cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitDHT) && port > 0) {
			cn.cfg.debug().Printf("posting dht not supported extension supported(%t) - port(%d)\n", cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitDHT), port)
			return n
		}

		defer cn.cfg.debug().Println("dht extension completed")

		_, err := ws.Post(btprotocol.NewPort(port))
		if err != nil {
			return cstate.Failure(err)
		}

		return n
	})
}

// newWriterState allocates the part of *writerstate that mainReadLoop needs
// to reach (via mutate/view) before the writer goroutine itself has started -
// so it must be constructed before RunHandshookConn spawns either goroutine,
// not inside connwriterinit. connwriterinit fills in the rest (below), all
// of which is genuinely writer-goroutine-exclusive since mainReadLoop never
// touches it.
func newWriterState(cn *connection) *writerstate {
	return &writerstate{
		connection:         cn,
		PeerChoked:         true, // peer has restricted us from making requests, until they say otherwise.
		PeerMaxRequests:    atomicx.Uint32(cn.cfg.maximumOutstandingRequests),
		PendingMaxRequests: cn.cfg.maximumOutstandingRequests,
		fastset:            roaring.New(),
		touched:            roaring.New(),
		requests:           make(map[uint64]request, cn.cfg.maximumOutstandingRequests),
		bufferLimit:        writebufferscapacity,
		buffer:             bytes.NewBuffer(make([]byte, 0, writebufferscapacity)),
		pool: sync.Pool{New: func() any {
			return bytes.NewBuffer(make([]byte, 0, writebufferscapacity))
		}},
	}
}

// Routine that writes to the peer. Some of what to write is buffered by
// activity elsewhere in the Client, and some is determined locally when the
// connection is writable.
func connwriterinit(ctx context.Context, ws *writerstate, to time.Duration) (err error) {
	cn := ws.connection
	cn.cfg.debug().Printf("c(%p) writer initiated\n", cn)
	defer cn.cfg.debug().Printf("c(%p) writer completed\n", cn)
	ctx, done := context.WithCancel(ctx)
	defer done()

	ts := time.Now()
	ws.keepAliveTimeout = to
	ws.chokeduntil = ts.Add(-1 * time.Minute)
	ws.nextbitmap = ts.Add(time.Minute)
	ws.keepaliverequired = atomicx.Pointer(ts.Add(to))
	ws.resyncbitfield = atomicx.Pointer(ts.Add(time.Minute))
	ws.lowrequestwatermark = max(1, int(ws.PeerMaxRequests.Load()/4))
	ws.requestable = roaring.New()
	ws.seed = cn.t.seeding()
	ws.Idler = cstate.Idle(ctx, cn.request, cn.t.chunks.cond)

	defer ws.Idler.Stop()
	defer ws.checkFailures()
	defer ws.mutate(func(ws *writerstate) { ws.deleteAllRequestsLocked() })

	return cstate.Run(ctx, connWriterInterested(ws, connwriterRequests(ws)), cn.cfg.debug())
}

type writerstate struct {
	*connection
	bufferLimit         int
	keepAliveTimeout    time.Duration
	nextbitmap          time.Time
	chokeduntil         time.Time
	seed                bool
	keepaliverequired   *atomic.Pointer[time.Time]
	resyncbitfield      *atomic.Pointer[time.Time]
	requestablecheck    uint64
	requestable         *roaring.Bitmap // represents the chunks we're currently allow to request.
	lowrequestwatermark int
	PeerMaxRequests     *atomic.Uint32 // Maximum pending requests the peer allows. Set during the extended handshake, written from mainReadLoop, read from the writer goroutine.
	PendingMaxRequests  int            // Maximum pending requests the client allows. Set during the extended handshake.
	*cstate.Idler

	// mu guards PeerChoked, fastset, touched, requests, currentbuffer, and
	// metadataRequests below. They're conceptually writer-owned (only the
	// writer goroutine's own code reads or acts on them meaningfully), but
	// mainReadLoop (and, pre-spawn, ConnExtensions) also needs to write to
	// them directly as protocol events arrive - so every access, including
	// the writer's own, goes through mutate/view rather than assuming
	// single-goroutine safety.
	mu         sync.RWMutex
	PeerChoked bool            // peer has restricted us from making requests.
	fastset    *roaring.Bitmap // represents chunks which our peer will allow us to request while choked.
	touched    *roaring.Bitmap // pieces we've accepted chunks for from the peer.
	// requests (what we've asked the peer for). requestcount mirrors its
	// length atomically so other goroutines can read the count without
	// taking mu.
	requests map[uint64]request
	// buffer holds messages queued but not yet flushed to the wire.
	// Written from the writer's own code, mainReadLoop (reject/PEX/metadata
	// requests posted in response to incoming messages), and, pre-spawn,
	// ConnExtensions - never by the reader goroutine directly.
	buffer *bytes.Buffer
	// pool recycles buffers swapped out by Flush, avoiding a fresh
	// allocation every flush cycle. Owned per-connection rather than
	// shared package-wide; safe for concurrent Get/Put on its own.
	pool sync.Pool
	// metadataRequests is indexed by metadata piece, true if posted and
	// pending a response. Written by the writer's own reactive per-cycle
	// check and by mainReadLoop (clearing a piece once its data arrives).
	metadataRequests []bool
}

// mutate runs op against the writer's synchronized state under an exclusive
// lock. Used by mainReadLoop (and anything else outside the writer
// goroutine) to write PeerChoked/fastset/touched/requests, and by the writer
// goroutine's own code for the same fields - see writerstate.mu.
func (ws *writerstate) mutate(op func(*writerstate)) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	op(ws)
}

// view runs op against the writer's synchronized state under a shared lock -
// the read counterpart to mutate.
func (ws *writerstate) view[T any](op func(*writerstate) T) T {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return op(ws)
}

func (ws *writerstate) peerChoked() (r bool) {
	return ws.view(func(ws *writerstate) bool { return ws.PeerChoked })
}

// clearRequestsLocked drops the given requests. Caller must hold ws.mu.
func (ws *writerstate) clearRequestsLocked(reqs ...request) (ok bool) {
	for _, r := range reqs {
		if _, present := ws.requests[r.Digest]; !present {
			continue
		}
		delete(ws.requests, r.Digest)
		ws.requestcount.Add(-1)
		ok = true
	}
	return ok
}

// releaseRequestLocked returns the request back to the chunks pool. Caller
// must hold ws.mu.
func (ws *writerstate) releaseRequestLocked(r request) (ok bool) {
	if r, ok = ws.requests[r.Digest]; !ok {
		return false
	}

	// ws.cfg.debug().Printf("c(%p) - releasing request d(%020d) r(%d,%d,%d)\n", ws, r.Digest, r.Index, r.Begin, r.Length)
	delete(ws.requests, r.Digest)
	ws.requestcount.Add(-1)
	ws.t.chunks.Retry(r)

	return true
}

// deleteAllRequestsLocked releases every outstanding request. Caller must
// hold ws.mu.
func (ws *writerstate) deleteAllRequestsLocked() {
	for _, r := range ws.requests {
		ws.releaseRequestLocked(r)
	}
}

// Write appends into currentbuffer. Called by the writer's own code, by
// mainReadLoop (via Post, reacting to incoming messages that require an
// immediate reply), and, pre-spawn, by ConnExtensions.
func (ws *writerstate) Write(encoded []byte) (n int, err error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.buffer.Write(encoded)
}

// Post encodes and writes a message into the write buffer.
func (ws *writerstate) Post(msg btprotocol.Message) (n int, err error) {
	encoded, err := msg.MarshalBinary()
	if err != nil {
		return n, errorsx.Wrapf(err, "failed to encode message %T", msg)
	}

	n, err = ws.Write(encoded)
	if err != nil {
		return n, errorsx.Wrapf(err, "failed to write message into buffer %T", msg)
	}

	ws.wroteMsg(&msg)
	ws.updateRequests()
	return n, nil
}

func (ws *writerstate) PostBitfield(dup *roaring.Bitmap) (n int, err error) {
	ws.cfg.debug().Printf("c(%p) seed(%t) calculated bitfield: b(%d)/p(%d)\n", ws.connection, ws.t.seeding(), dup.GetCardinality(), ws.t.chunks.pieces)
	n, err = ws.Post(btprotocol.NewBitField(ws.t.chunks.pieces, dup))
	return n, err
}

// Flush swaps out currentbuffer for a fresh buffer, rather than resetting it
// in place, so the bytes handed to FlushBuffer below are exclusively owned
// by this call. bytes.Buffer.Bytes() returns a view into the buffer's live
// backing array, and Reset() does not discard that array - resetting
// currentbuffer in place and then writing its (former) Bytes() outside the
// lock would let a concurrent Write silently overwrite the same memory
// before the actual network write reads it, corrupting the wire bytes sent
// to the peer.
func (ws *writerstate) Flush() (int, error) {
	ws.mu.Lock()
	buf := ws.buffer
	next := ws.pool.Get().(*bytes.Buffer)
	next.Reset()
	ws.buffer = next
	ws.mu.Unlock()

	defer ws.pool.Put(buf)
	return ws.FlushBuffer(buf.Bytes())
}

func (ws *writerstate) requestedMetadataPiece(index int) bool {
	return ws.view(func(ws *writerstate) bool {
		return index < len(ws.metadataRequests) && ws.metadataRequests[index]
	})
}

func (ws *writerstate) requestMetadataPiece(index int) {
	if ws.requestedMetadataPiece(index) {
		return
	}

	cn := ws.connection
	encoded, err := bencode.Marshal(bep0009.MetadataRequest{
		Type:  btprotocol.RequestMetadataExtensionMsgType,
		Index: index,
	})
	if err != nil {
		log.Println("able to encoded metadata request", err)
		return
	}

	if _, err := ws.Post(btprotocol.NewExtended(cn.extension(btprotocol.ExtensionNameMetadata), encoded)); err != nil {
		log.Println("able to post metadata request", err)
		return
	}

	ws.mutate(func(ws *writerstate) {
		for index >= len(ws.metadataRequests) {
			ws.metadataRequests = append(ws.metadataRequests, false)
		}
		ws.metadataRequests[index] = true
	})
}

// requestPendingMetadata is called reactively on every writer cycle (see
// _connwriterRequests.Update) rather than only in direct response to this
// connection's own extended handshake - metadata size can also become known
// via a different connection entirely (torrent.setMetadataSize), which has
// no way to reach another connection's writer state directly, so it just
// broadcasts cn.request for every connection instead and leaves the actual
// work to each connection's own next cycle here.
func (ws *writerstate) requestPendingMetadata() {
	cn := ws.connection
	if !cn.extensionEnabled(btprotocol.ExtensionNameMetadata) || cn.t.haveInfo() {
		return
	}

	// Request metadata pieces that we don't have in a random order.
	var pending []int
	for index := 0; index < cn.t.metadataPieceCount(); index++ {
		if !cn.t.haveMetadataPiece(index) && !ws.requestedMetadataPiece(index) {
			pending = append(pending, index)
		}
	}

	if len(pending) == 0 {
		return
	}

	rand.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	for _, i := range pending {
		ws.requestMetadataPiece(i)
	}
}

func (ws *writerstate) checkFailures() error {
	if ws.t.chunks.FailedEmpty() {
		return nil
	}

	return ws.view(func(ws *writerstate) error {
		if ws.touched.IsEmpty() {
			return nil
		}

		failed := ws.t.chunks.Failed(ws.touched.Clone())

		for iter, prev, pid := failed.ReverseIterator(), -1, 0; iter.HasNext(); prev = pid {
			pid = ws.t.chunks.pindex(int(iter.Next()))
			if pid == prev {
				continue
			}

			ws.stats.PiecesDirtiedBad.Add(1)
			if !ws.t.chunks.ChunksComplete(uint64(pid)) {
				ws.t.chunks.ChunksRetry(uint64(pid))
			}
		}

		if !ws.trusted && ws.stats.PiecesDirtiedBad.Int64() > 10 {
			return connections.NewBanned(ws.conn, false, errorsx.New("too many bad pieces"))
		}

		return nil
	})
}

func (t *writerstate) bufmsg(msg btprotocol.Message) error {
	if _, err := t.Write(msg.MustMarshalBinary()); err != nil {
		return err
	}

	t.wroteMsg(&msg)

	if n := t.view(wsBufferLen); n < t.bufferLimit {
		return nil
	} else {
		return errorsx.Errorf("maximum capacity %d < %d", n, t.bufferLimit)
	}
}

func (t *writerstate) String() string {
	return fmt.Sprintf("c(%p) seed(%t)", t.connection, t.connection.t.seeding())
}

func connWriterSyncBitfield(ws *writerstate, next cstate.T) cstate.T {
	return _connWriterSyncBitfield{writerstate: ws, next: next}
}

type _connWriterSyncBitfield struct {
	*writerstate
	next cstate.T
}

func (t _connWriterSyncBitfield) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate

	if ts := ws.resyncbitfield.Load(); ts.After(time.Now()) {
		return t.next
	}

	ws.resyncbitfield.Store(langx.Autoptr(time.Now().Add(time.Minute)))

	dup := ws.t.chunks.Clone(ws.t.chunks.completed)
	dup.AndNot(ws.sentHaves)

	for i := dup.Iterator(); i.HasNext(); {
		err := t.bufmsg(btprotocol.NewHavePiece(uint64(i.Next())))
		if err != nil {
			return cstate.Failure(err)
		}
	}

	ws.sentHaves.Or(dup)

	if !dup.IsEmpty() {
		ws.t.readabledataavailable.Store(true)
	}

	return t.next
}

func connwriterclosed(ws *writerstate, next cstate.T) cstate.T {
	return _connWriterClosed{writerstate: ws, next: next}
}

type _connWriterClosed struct {
	*writerstate
	next cstate.T
}

func (t _connWriterClosed) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate

	// delete requests that were requested beyond the timeout.
	timedout := func(cn *connection, grace time.Duration) bool {
		return ws.requestsLen() > 0 && cn.lastUsefulChunkReceived.Load().Add(grace).Before(time.Now()) && cn.t.chunks.Cardinality(cn.t.chunks.missing) > 0
	}

	if ws.closed.Load() {
		return nil
	}

	// if we're choked and not allowed to fast track any chunks then there is nothing
	// to do.
	if ws.view(func(ws *writerstate) bool { return ws.PeerChoked && ws.fastset.IsEmpty() }) {
		return connwriterFlush(t.next, ws)
	}

	// detect effectively dead connections, choking them for at least 1 minute.
	if timedout(ws.connection, ws.t.chunks.gracePeriod) {
		if err := ws.Choke(ws.bufmsg, ws); err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "c(%p) peer isnt sending chunks in a timely manner requests (%d > %d) last(%s) and we failed to choke them", ws, ws.requestsLen(), ws.PeerMaxRequests.Load(), time.Since(*ws.lastUsefulChunkReceived.Load())))
		}

		ts := time.Now()

		if ws.chokeduntil.Add(time.Minute).Before(ts) {
			ws.chokeduntil = ts.Add(backoffx.DynamicHash1m(ws.PeerID.String()) + backoffx.Random(10*time.Minute))
		} else if d := time.Since(*ws.lastUsefulChunkReceived.Load()); d > 4*ws.t.chunks.gracePeriod {
			return cstate.Failure(
				errorsx.Timedout(
					errorsx.Errorf("c(%p) peer did not send chunks in a timely manner requests (%d > %d) last(%s)", ws, ws.requestsLen(), ws.PeerMaxRequests.Load(), d),
					time.Minute,
				),
			)
		}

		return cstate.Warning(
			ws.Idler.Idle(t.next, 5*time.Second),
			errorsx.Errorf("c(%p) peer isnt sending chunks in a timely manner requests (%d > %d) last(%s)", ws, ws.requestsLen(), ws.PeerMaxRequests.Load(), time.Since(*ws.lastUsefulChunkReceived.Load())),
		)
	}

	return t.next
}

func connWriterInterested(ws *writerstate, next cstate.T) cstate.T {
	return _connWriterInterested{writerstate: ws, next: next}
}

type _connWriterInterested struct {
	*writerstate
	next cstate.T
}

func (t _connWriterInterested) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate
	if ws.t.chunks.Incomplete() {
		return t.next
	}

	ws.SetInterested(false, messageWriter(ws.bufmsg).Deprecated())
	return t.next
}

func connwriterRequests(ws *writerstate) cstate.T {
	return _connwriterRequests{writerstate: ws}
}

type _connwriterRequests struct {
	*writerstate
}

func (t _connwriterRequests) determineInterest(msg messageWriter) *roaring.Bitmap {
	// defer t.cfg.debug().Printf("c(%p) seed(%t) interest completed requestable(%d)\n", t.connection, t.seed, t.requestable.GetCardinality())
	if t.seed || t.chokeduntil.After(time.Now()) {
		if t.Unchoke(msg.Deprecated()) {
			t.cfg.debug().Printf("c(%p) seed(%t) allowing peer to make requests\n", t.connection, t.seed)
		}
	} else {
		if t.Choke(msg, t.writerstate) == nil {
			t.cfg.debug().Printf("c(%p) seed(%t) disallowing peer to make requests\n", t.connection, t.seed)
		}
	}

	if ts := langx.Zero(t.refreshrequestable.Load()); ts.After(time.Now()) {
		t.cfg.debug().Printf("c(%p) seed(%t) allowing cached %d - %s\n", t.connection, t.seed, t.requestable.GetCardinality(), time.Until(ts))
		return t.requestable
	}

	if m := t.t.chunks.Cardinality(t.t.chunks.completed); uint64(m) == t.t.chunks.pieces {
		t.cfg.debug().Printf("c(%p) seed(%t) disabling requestable - have all data m(%d) o(%d) c(%d) p(%d)\n", t.connection, t.seed, m, len(t.t.chunks.outstanding), m, t.t.chunks.pieces)
		t.refreshrequestable.Store(langx.Autoptr(timex.Inf()))
		t.requestable = roaring.New()
		return t.requestable
	}

	t.cfg.debug().Printf("c(%p) seed(%t) refreshing availability\n", t.connection, t.seed)

	fastset := t.view(func(ws *writerstate) *roaring.Bitmap {
		return ws.fastset.Clone()
	})
	peerChoked := t.view(func(ws *writerstate) bool {
		return ws.PeerChoked
	})

	claimed := roaring.New()

	if !peerChoked {
		t._mu.RLock()
		// t.cfg.debug().Printf("c(%p) seed(%t) allowing claimed: %d\n", t.connection, t.seed, t.claimed.GetCardinality())
		claimed = t.claimed.Clone()
		t._mu.RUnlock()
	}

	t.refreshrequestable.Store(langx.Autoptr(timex.Inf()))

	tmp := bitmapx.Fill(t.t.chunks.cmaximum)
	tmp.AndAny(fastset, claimed)

	// check if the bitmap changed from the previous refresh.
	// we do this because refresh events an come in asynchronously
	// which do not actually alter the computed bitmap resulting
	// in duplicate requests when outstanding requests are repeated
	// due to the reset of the requestable bitmap that occurrs here.
	tsum := tmp.Checksum()
	if tsum == t.requestablecheck {
		return t.requestable
	}
	t.requestablecheck = tsum
	t.requestable = tmp

	if !t.SetInterested(!t.requestable.IsEmpty(), msg.Deprecated()) {
		t.cfg.debug().Printf("c(%p) seed(%t) nothing available to request %d\n", t.connection, t.seed, t.requestable.GetCardinality())
		return t.requestable
	}

	if t.connection.t.chunks.missing.GetCardinality() == 0 && t.connection.t.chunks.unverified.GetCardinality() > 0 {
		t.connection.t.digests.EnqueueBitmap(bitmapx.Fill(t.connection.t.chunks.pieces))
	}

	return t.requestable
}

// Proxies the messageWriter's response.
func (t _connwriterRequests) request(r request, mw messageWriter) bool {
	t.mutate(func(ws *writerstate) {
		ws.requests[r.Digest] = r
		ws.requestcount.Add(1)
	})

	return mw(btprotocol.Message{
		Type:   btprotocol.Request,
		Index:  r.Index,
		Begin:  r.Begin,
		Length: r.Length,
	}) == nil
}

func (t _connwriterRequests) genrequests(available *roaring.Bitmap, msg messageWriter) {
	var (
		err         error
		reqs        []request
		req         request
		unavailable empty
	)

	// t.cfg.debug().Printf("c(%p) seed(%t) make requests initated avail(%d)\n", t.connection, t.seed, t.requestable.GetCardinality())
	// defer t.cfg.debug().Printf("c(%p) seed(%t) make requests completed avail(%d)\n", t.connection, t.seed, t.requestable.GetCardinality())

	if t.requestsLen() > t.lowrequestwatermark {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - req(current(%d) >= low watermark(%d) / 2)", t.connection, t.seed, t.requestsLen(), t.lowrequestwatermark)
		return
	}

	if unmodified := !t.refreshrequestable.Load().Before(time.Now()); available.IsEmpty() && unmodified {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - avail(%d) && unmodified(%t)", t.connection, t.seed, available.GetCardinality(), unmodified)
		return
	}

	// once we fall below the low watermark dynamic adjust it based on what we saw.
	// never allowing it to go above the original low watermark and with a floor of a single request.
	t.lowrequestwatermark += min(1, int(t.chunksReceived.Swap(0)*4-t.chunksRejected.Swap(0)))
	t.lowrequestwatermark = min(t.lowrequestwatermark, int(t.PeerMaxRequests.Load()))

	max := max(0, t.lowrequestwatermark-t.requestsLen())
	if reqs, err = t.t.chunks.Pop(max, available); errors.As(err, &unavailable) {
		if len(reqs) == 0 && t.requestable.IsEmpty() && (unavailable.Missing > 0 || unavailable.Outstanding > 0) {
			// mark out available set for refresh when we hit this state.
			// this is because we remove chunks from our requestable set before we receive them.
			// and when we run out of work and there is more things to request it means we missed some.
			t.refreshrequestable.Store(langx.Autoptr(time.Now()))

			t.t.chunks.MergeInto(t.t.chunks.missing, t.t.chunks.failed)
			t.t.chunks.FailuresReset()
			t.cfg.debug().Printf("c(%p) seed(%t) available(%t) no work available - scheduled requestable update", t.connection, t.seed, !available.IsEmpty())
			return
		}
	} else if err != nil {
		t.cfg.errors().Printf("failed to request piece: %T - %v\n", err, err)
		return
	}

	// t.cfg.debug().Printf("c(%p) seed(%t) avail(%d) filling buffer with requests low(%d) - max(%d) outstanding(%d) -> allowed(%d) actual %d", t.connection, t.seed, available.GetCardinality(), t.lowrequestwatermark, t.PeerMaxRequests, t.requestsLen(), max, len(reqs))

	for max, req = range reqs {
		if filledBuffer := !t.request(req, msg); filledBuffer {
			t.cfg.debug().Printf("c(%p) seed(%t) done filling after(%d)\n", t.connection, t.seed, max)
			break
		}

		// remove requests that have been requested to prevent them from
		// being requested from this connection until requestable is recalculated.
		// which happens whenever we run out of chunks to request.
		t.requestable.Remove(uint32(t.t.chunks.requestCID(req)))

		// t.cfg.debug().Printf("c(%p) seed(%t) choked(%t) requested(%d, %d, %d) remaining(%d)\n", t.connection, t.seed, t.PeerChoked, req.Index, req.Begin, req.Length, t.requestable.GetCardinality())
	}

	// advance to just the unused chunks.
	if max += 1; len(reqs) > max {
		reqs = reqs[max:]
		t.cfg.debug().Printf("c(%p) seed(%t) filled - cleaning up %d reqs(%d)\n", t.connection, t.seed, max, len(reqs))
		// release any unused requests back to the queue.
		t.t.chunks.Retry(reqs...)
	}
}

func (t _connwriterRequests) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate

	if err := ws.checkFailures(); err != nil {
		return cstate.Failure(err)
	}

	ws.requestPendingMetadata()

	t.genrequests(t.determineInterest(ws.bufmsg), ws.bufmsg)

	// needresponse is tracking read that come in while we're in the critical section of this function
	// to prevent the state machine from going idle just because we didnt write anything this cycle.
	// needresponse tracks that a message can in that requires a message be sent.
	if ws.view(wsBufferLen) > 0 {
		return connwriterFlush(
			connwriteractive(ws),
			ws,
		)
	}

	return connwriteridle(ws)
}

func connwriterKeepalive(ws *writerstate, n cstate.T) cstate.T {
	return _connwriterKeepalive{writerstate: ws, next: n}
}

type _connwriterKeepalive struct {
	*writerstate
	next cstate.T
}

func (t _connwriterKeepalive) Update(ctx context.Context, _ *cstate.Shared) cstate.T {
	var (
		err error
	)

	ws := t.writerstate

	if langx.Zero(ws.keepaliverequired.Load()).After(time.Now()) {
		return t.next
	}

	if _, err = ws.Post(btprotocol.NewKeepAlive()); err != nil {
		return cstate.Failure(errorsx.Wrap(err, "keepalive encoding failed"))
	}

	ws.keepaliverequired.Store(langx.Autoptr(time.Now().Add(ws.keepAliveTimeout)))

	return connwriterFlush(t.next, ws)
}

func connwriterFlush(n cstate.T, ws *writerstate) cstate.T {
	return _connwriterFlush{next: n, writerstate: ws}
}

type _connwriterFlush struct {
	next cstate.T
	*writerstate
}

func (t _connwriterFlush) Update(ctx context.Context, _ *cstate.Shared) cstate.T {
	var (
		err error
	)

	ws := t.writerstate
	n, err := t.Flush()

	if err != nil {
		return cstate.Failure(errorsx.Wrap(err, "failed to send requests"))
	}

	if n != 0 {
		ws.keepaliverequired.Store(langx.Autoptr(time.Now().Add(ws.keepAliveTimeout)))
	}

	return t.next
}

func connwriterBitmap(ws *writerstate, n cstate.T) cstate.T {
	return _connwriterCommitBitmap{next: n, writerstate: ws}
}

type _connwriterCommitBitmap struct {
	next cstate.T
	*writerstate
}

func (t _connwriterCommitBitmap) Update(ctx context.Context, _ *cstate.Shared) cstate.T {
	ws := t.writerstate

	if ws.nextbitmap.After(time.Now()) {
		return t.next
	}

	defer func() {
		ws.nextbitmap = time.Now().Add(time.Minute)
	}()

	if err := ws.t.cln.torrents.Sync(int160.FromBytes(ws.t.md.ID.Bytes())); err != nil {
		return cstate.Warning(t.next, errorsx.Wrap(err, "failed to sync bitmap to disk"))
	}

	return t.next
}

func connwriteridle(ws *writerstate) cstate.T {
	now := time.Now()
	keepalive := now.Add(ws.keepAliveTimeout / 2)

	if ws.needsresponse.CompareAndSwap(true, false) {
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle downloads(%t) %s - needs response\n", ws.connection, ws.t.seeding(), !ws.peerChoked(), ws.t.chunks)
		return connwriteractive(ws)
	}

	ts := []time.Time{
		keepalive,
		ws.nextbitmap,
		timex.Max(ws.chokeduntil, keepalive),
		langx.Zero(ws.keepaliverequired.Load()),
		langx.Zero(ws.refreshrequestable.Load()),
	}
	mind := time.Until(timex.Min(ts...))

	if mind <= 0 {
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle downloads(%t) %s - %s\n", ws.connection, ws.t.seeding(), !ws.peerChoked(), ws.t.chunks, mind)
		return connwriteractive(ws)
	}

	ws.cfg.debug().Printf("c(%p) seed(%t) idling downloads(%t) %s - %s\n", ws.connection, ws.t.seeding(), !ws.peerChoked(), ws.t.chunks, mind)
	return connWriterSyncBitfield(ws, connWriterInterested(ws, ws.Idler.Idle(connwriteractive(ws), mind)))
}

func connwriteractive(ws *writerstate) cstate.T {
	return connwriterKeepalive(ws, connwriterclosed(ws, connwriterBitmap(ws, connWriterInterested(ws, connwriterRequests(ws)))))
}

// wsBufferLen returns the length of the pending outgoing buffer.
func wsBufferLen(ws *writerstate) int {
	return ws.buffer.Len()
}
