package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/bep0006"
	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/cstate"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/backoffx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"github.com/james-lawrence/torrent/internal/timex"
	"github.com/james-lawrence/torrent/internal/x/bitmapx"
)

func RunHandshookConn(c *connection, t *torrent) error {
	const retrydelay = 10 * time.Second

	remotreaddr := c.conn.RemoteAddr()
	c.setTorrent(t)

	c.conn.SetWriteDeadline(time.Time{})
	c.r = deadlineReader{c.conn, c.r}
	completedHandshakeConnectionFlags.Add(c.connectionFlags(), 1)

	defer t.event.Broadcast()
	defer t.dropConnection(c)

	if err := t.addConnection(c); err != nil {
		return errorsx.Wrap(err, "error adding connection")
	}

	defer c.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	c.cfg.debug().Printf("%p exchanging extensions\n", c)

	if err := ConnExtensions(ctx, c); err != nil {
		err = errorsx.LogErr(errorsx.Wrap(err, "error sending configuring connection"))
		cancel(err)
		return err
	}

	go func() {
		err := connwriterinit(ctx, c, 10*time.Second)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		cancel(errorsx.LogErr(err))
		c.Close()
	}()

	if err := c.mainReadLoop(ctx); err != nil {
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
func ConnExtensions(ctx context.Context, cn *connection) error {
	cn.cfg.debug().Println("conn extensions initiated")
	defer cn.cfg.debug().Println("conn extensions completed")
	return cstate.Run(ctx, connexinit(cn, connexfast(cn, connexdht(cn, connflush(cn, nil)))), cn.cfg.debug())
}

func connflush(cn *connection, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		_, err := cn.Flush()
		if err != nil {
			return cstate.Failure(errorsx.Wrap(err, "failed to send requests"))
		}
		return n
	})
}

func connexinit(cn *connection, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		if !cn.extensions.Supported(cn.PeerExtensionBytes, pp.ExtensionBitExtended) {
			return n
		}

		dynamicport := langx.DefaultIfZero(cn.localport, langx.Autoderef(cn.dynamicaddr.Load()).Port())
		defer cn.cfg.debug().Println("extended handshake extension completed", cn.localport, cn.dynamicaddr)

		// TODO: We can figured the port and address out specific to the socket
		// used.
		msg := pp.ExtendedHandshakeMessage{
			M:            cn.cfg.extensions,
			V:            cn.cfg.ExtendedHandshakeClientVersion,
			Reqq:         cn.cfg.maximumOutstandingRequests,
			YourIp:       pp.CompactIp(cn.remoteAddr.Addr().AsSlice()),
			Encryption:   cn.cfg.HeaderObfuscationPolicy.Preferred || cn.cfg.HeaderObfuscationPolicy.RequirePreferred,
			Port:         int(dynamicport),
			MetadataSize: cn.t.metadatalen(),
			Ipv4:         pp.CompactIp(cn.cfg.publicIP4.To4()),
			Ipv6:         cn.cfg.publicIP6.To16(),
		}

		// cn.cfg.debug().Printf("c(%p) seed(%t) extended handshake: %s\n", cn, cn.t.seeding(), spew.Sdump(msg))

		encoded, err := bencode.Marshal(msg)
		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		_, err = cn.Post(pp.NewExtendedHandshake(encoded))

		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		return n
	})
}

func connexfast(cn *connection, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		defer cn.cfg.debug().Printf("c(%p) seed(%t) fast extension completed\n", cn, cn.t.seeding())
		if !cn.supported(pp.ExtensionBitFast) {
			if _, err := cn.PostBitfield(); err != nil {
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
			if _, err := cn.Post(pp.NewHaveNone()); err != nil {
				return cstate.Failure(err)
			}
			cn.sentHaves.Clear()
			return n
		case uint64(cn.t.chunks.cmaximum):
			cn.cfg.debug().Printf("c(%p) seed(%t) posting allow fast have all: %d/%d\n", cn, cn.t.seeding(), readable, cn.t.chunks.cmaximum)
			if _, err := cn.Post(pp.NewHaveAll()); err != nil {
				return cstate.Failure(err)
			}

			cn.sentHaves.AddRange(0, cn.t.chunks.pieces)

			for _, v := range cn.peerfastset.ToArray() {
				if _, err := cn.Post(pp.NewAllowedFast(v)); err != nil {
					return cstate.Failure(err)
				}
			}

			return n
		default:
			cn.cfg.debug().Printf("c(%p) seed(%t) posting bitfield: %d/%d\n", cn, cn.t.seeding(), readable, cn.t.chunks.cmaximum)
			if _, err := cn.PostBitfield(); err != nil {
				return cstate.Failure(err)
			}
		}

		return n
	})
}

func connexdht(cn *connection, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		dynamicaddr := langx.Autoderef(cn.dynamicaddr.Load())
		port := langx.DefaultIfZero(cn.t.cln.LocalPort16(), dynamicaddr.Port())
		if !(cn.extensions.Supported(cn.PeerExtensionBytes, pp.ExtensionBitDHT) && port > 0) {
			cn.cfg.debug().Printf("posting dht not supported extension supported(%t) - port(%d)\n", cn.extensions.Supported(cn.PeerExtensionBytes, pp.ExtensionBitDHT), port)
			return n
		}

		defer cn.cfg.debug().Println("dht extension completed")

		_, err := cn.Post(pp.NewPort(port))
		if err != nil {
			return cstate.Failure(err)
		}

		return n
	})
}

// Routine that writes to the peer. Some of what to write is buffered by
// activity elsewhere in the Client, and some is determined locally when the
// connection is writable.
func connwriterinit(ctx context.Context, cn *connection, to time.Duration) (err error) {
	cn.cfg.debug().Printf("c(%p) writer initiated\n", cn)
	defer cn.cfg.debug().Printf("c(%p) writer completed\n", cn)
	defer func() {
		// if we're closed that means the connection was shutdown
		// and since we're just returning that means someone else already detected
		// and reported the issue.
		if cn.closed.Load() {
			err = nil
		}
	}()

	ts := time.Now()
	ws := &writerstate{
		bufferLimit:         64 * bytesx.KiB,
		connection:          cn,
		keepAliveTimeout:    to,
		chokeduntil:         ts.Add(-1 * time.Minute),
		nextbitmap:          ts.Add(time.Minute),
		refreshavailable:    atomicx.Pointer(ts),
		uploadavailable:     atomicx.Pointer(ts),
		keepaliverequired:   atomicx.Pointer(ts.Add(to)),
		lowrequestwatermark: max(1, cn.PeerMaxRequests/4),
	}

	defer cn.checkFailures()
	defer cn.deleteAllRequests()

	return errorsx.LogErr(cstate.Run(ctx, connWriterSyncChunks(ws), cn.cfg.debug()))
}

type writerstate struct {
	*connection
	bufferLimit         int
	keepAliveTimeout    time.Duration
	nextbitmap          time.Time
	chokeduntil         time.Time
	keepaliverequired   *atomic.Pointer[time.Time]
	refreshavailable    *atomic.Pointer[time.Time]
	uploadavailable     *atomic.Pointer[time.Time]
	cachedavailable     *roaring.Bitmap
	lowrequestwatermark int
}

func (t *writerstate) String() string {
	return fmt.Sprintf("c(%p) seed(%t)", t.connection, t.connection.t.seeding())
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
		return len(ws.requests) > 0 && cn.lastUsefulChunkReceived.Add(grace).Before(time.Now()) && cn.t.chunks.Cardinality(cn.t.chunks.missing) > 0
	}

	if ws.closed.Load() {
		return nil
	}

	if ts := *ws.lastMessageReceived.Load(); time.Since(ts) > 2*ws.keepAliveTimeout {
		return cstate.Failure(
			errorsx.Timedout(
				fmt.Errorf("connection timed out %s %v %v", ws.remoteAddr.String(), time.Since(ts), ts),
				10*time.Second,
			),
		)
	}

	// if we're choked and not allowed to fast track any chunks then there is nothing
	// to do.
	if ws.PeerChoked && ws.peerfastset.IsEmpty() {
		return connwriterFlush(connwriteridle(ws), ws)
	}

	// detect effectively dead connections, choking them for at least 1 minute.
	if timedout(ws.connection, ws.t.chunks.gracePeriod) {
		if _, err := ws.PostImmediate(pp.NewChoked()); err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "c(%p) peer isnt sending chunks in a timely manner requests (%d > %d) last(%s) and we failed to choke them", ws, len(ws.requests), ws.PeerMaxRequests, ws.lastUsefulChunkReceived))
		}

		ws.chokeduntil = time.Now().Add(backoffx.DynamicHash1m(ws.PeerID.String()) + backoffx.Random(10*time.Minute))

		return cstate.Warning(
			t.next,
			errorsx.Errorf("c(%p) peer isnt sending chunks in a timely manner requests (%d > %d) last(%s)", ws, len(ws.requests), ws.PeerMaxRequests, ws.lastUsefulChunkReceived),
		)
	}

	return t.next
}

func connWriterSyncChunks(ws *writerstate) cstate.T {
	return _connWriterSyncChunks{writerstate: ws}
}

type _connWriterSyncChunks struct {
	*writerstate
}

func (t _connWriterSyncChunks) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate
	next := connWriterSyncComplete(ws)

	if ws.keepaliverequired.Load().After(time.Now()) {
		return next
	}

	dup := ws.t.chunks.completed.Clone()
	dup.AndNot(ws.sentHaves)

	for i := dup.Iterator(); i.HasNext(); {
		piece := i.Next()
		ws.cmu().Lock()
		added := ws.sentHaves.CheckedAdd(piece)
		ws.cmu().Unlock()
		if !added {
			continue
		}

		_, err := ws.Post(pp.NewHavePiece(uint64(piece)))
		if err != nil {
			return cstate.Failure(err)
		}
	}

	if !dup.IsEmpty() {
		ws.t.readabledataavailable.Store(true)
	}

	return next
}

func connWriterSyncComplete(ws *writerstate) cstate.T {
	return _connWriterSyncComplete{writerstate: ws}
}

type _connWriterSyncComplete struct {
	*writerstate
}

func (t _connWriterSyncComplete) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate
	next := connwriterRequests(ws)

	if ws.t.chunks.Incomplete() {
		return next
	}

	writer := func(msg pp.Message) bool {
		if _, err := ws.Write(msg.MustMarshalBinary()); err != nil {
			ws.cfg.errors().Println(err)
			return false
		}

		ws.wroteMsg(&msg)
		return ws.writeBuffer.Len() < ws.bufferLimit
	}

	ws.SetInterested(false, writer)
	return next
}

func connwriterRequests(ws *writerstate) cstate.T {
	return _connwriterRequests{writerstate: ws}
}

type _connwriterRequests struct {
	*writerstate
}

func (t _connwriterRequests) determineInterest(msg func(pp.Message) bool) (available *roaring.Bitmap) {
	defer t.cfg.debug().Printf("c(%p) seed(%t) interest completed\n", t.connection, t.t.seeding())

	if t.t.seeding() || t.chokeduntil.After(time.Now()) {
		if t.Unchoke(msg) {
			t.cfg.debug().Printf("c(%p) seed(%t) allowing peer to make requests\n", t.connection, t.t.seeding())
		}
	} else {
		if t.Choke(msg) {
			t.cfg.debug().Printf("c(%p) seed(%t) disallowing peer to make requests\n", t.connection, t.t.seeding())
		}
	}

	if !t.SetInterested(t.peerHasWantedPieces(), msg) {
		t.cfg.debug().Printf("c(%p) seed(%t) nothing available to request\n", t.connection, t.t.seeding())
	}

	if ts := langx.Autoderef(t.refreshavailable.Load()); ts.After(time.Now()) {
		t.cfg.debug().Printf("c(%p) seed(%t) allowing cached %d - %s\n", t.connection, t.t.seeding(), t.cachedavailable.GetCardinality(), time.Until(ts))
		return t.cachedavailable
	}

	if !t.PeerChoked {
		t.cfg.debug().Printf("c(%p) seed(%t) allowing claimed: %d\n", t.connection, t.t.seeding(), t.claimed.GetCardinality())
		available = t.claimed
	} else {
		t.cfg.debug().Printf("c(%p) seed(%t) allowing fastset %d\n", t.connection, t.t.seeding(), t.fastset.GetCardinality())
		available = t.fastset
	}

	t._mu.RLock()
	defer t._mu.RUnlock()

	t.cachedavailable = bitmapx.AndNot(available, t.blacklisted)
	return t.cachedavailable
}

func (t _connwriterRequests) genrequests(available *roaring.Bitmap, msg func(pp.Message) bool) {
	var (
		err  error
		reqs []request
		req  request
	)

	t.cfg.debug().Printf("c(%p) seed(%t) make requests initated\n", t.connection, t.t.seeding())
	defer t.cfg.debug().Printf("c(%p) seed(%t) make requests completed\n", t.connection, t.t.seeding())

	if available.IsEmpty() || len(t.requests) > t.lowrequestwatermark {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - avail(%d) || req(current(%d) > low watermark(%d))", t.connection, t.t.seeding(), available.GetCardinality(), len(t.requests), t.lowrequestwatermark)
		return
	}

	// once we fall below the low watermark dynamic adjust it based on what we saw.
	// never allowing it to go above the original low watermark and with a floor of a single request.
	t.lowrequestwatermark += int(t.chunksReceived.Swap(0)*4 - t.chunksRejected.Swap(0))
	t.lowrequestwatermark = max(1, min(t.lowrequestwatermark, t.PeerMaxRequests-(t.PeerMaxRequests/4)))

	if m := t.t.chunks.Cardinality(t.t.chunks.completed); uint64(m) == t.t.chunks.pieces {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - have all data m(%d) o(%d) c(%d) p(%d)\n", t.connection, t.t.seeding(), m, len(t.t.chunks.outstanding), t.t.chunks.Cardinality(t.t.chunks.completed), t.t.chunks.pieces)
		return
	}

	if m := t.t.chunks.Cardinality(t.t.chunks.missing); m == 0 {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - nothing to request m(%d) o(%d) c(%d) p(%d)\n", t.connection, t.t.seeding(), m, len(t.t.chunks.outstanding), t.t.chunks.Cardinality(t.t.chunks.completed), t.t.chunks.pieces)
		return
	}

	filledBuffer := false

	max := max(0, min(t.lowrequestwatermark, t.PeerMaxRequests)-len(t.requests))
	if reqs, err = t.t.chunks.Pop(max, available); errors.As(err, &empty{}) {
		if len(reqs) == 0 && !t.blacklisted.IsEmpty() {
			// clear the blacklist when we run out of work to do.
			t.cmu().Lock()
			t.blacklisted.Clear()
			t.cmu().Unlock()
			t.t.chunks.MergeInto(t.t.chunks.missing, t.t.chunks.failed)
			t.t.chunks.FailuresReset()
			t.cfg.debug().Printf("c(%p) seed(%t) available(%t) no work available", t.connection, t.t.seeding(), !available.IsEmpty())
			return
		}
	} else if err != nil {
		t.cfg.errors().Printf("failed to request piece: %T - %v\n", err, err)
		return
	}

	t.cfg.debug().Printf("c(%p) seed(%t) avail(%d) filling buffer with requests low(%d) - max(%d) outstanding(%d) -> allowed(%d) actual %d", t.connection, t.t.seeding(), available.GetCardinality(), t.lowrequestwatermark, t.PeerMaxRequests, len(t.requests), max, len(reqs))

	for max, req = range reqs {
		if filledBuffer = !t.request(req, msg); filledBuffer {
			t.cfg.debug().Printf("c(%p) seed(%t) done filling after(%d)\n", t.connection, t.t.seeding(), max)
			break
		}

		// cn.cfg.debug().Printf("c(%p) seed(%t) choked(%t) requested(%d, %d, %d)\n", cn, cn.t.seeding(), cn.PeerChoked, req.Index, req.Begin, req.Length)
	}

	// advance to just the unused chunks.
	if max += 1; len(reqs) > max {
		reqs = reqs[max:]
		t.cfg.debug().Printf("c(%p) seed(%t) filled - cleaning up %d reqs(%d)\n", t.connection, t.t.seeding(), max, len(reqs))
		// release any unused requests back to the queue.
		t.t.chunks.Retry(reqs...)
	}

	// If we didn't completely top up the requests, we shouldn't mark
	// the low water, since we'll want to top up the requests as soon
	// as we have more write buffer space.
	if !filledBuffer {
		t.requestsLowWater = len(t.requests) / 2
	}
}

// Also handles choking and unchoking of the remote peer.
func (t _connwriterRequests) upload(msg func(pp.Message) bool) (time.Duration, error) {
	// TODO: we should reject requests
	if t.Choked || t.peerSentHaveAll {
		t.cfg.debug().Printf("c(%p) seed(%t) choked(%t) peer completed(%t) req(%d) upload restricted - disallowed\n", t.connection, t.t.seeding(), t.Choked, t.peerSentHaveAll, len(t.PeerRequests))
		return time.Minute, nil
	}

	if ts, n := langx.Autoderef(t.uploadavailable.Load()), time.Now(); ts.After(n) {
		return time.Until(ts), nil
	}

	t.cfg.debug().Printf("c(%p) seed(%t) req(%d) uploading - allowed\n", t.connection, t.t.seeding(), len(t.PeerRequests))
	defer t.cfg.debug().Printf("c(%p) seed(%t) req(%d) uploading - competed\n", t.connection, t.t.seeding(), len(t.PeerRequests))
	uploaded := 0
	for r := range t.requestseq() {
		res := t.cfg.UploadRateLimiter.ReserveN(time.Now(), int(r.Length))
		if !res.OK() {
			t.cfg.debug().Printf("upload rate limiter burst size < %d\n", r.Length)
			return 0, connections.NewBanned(t.conn, errorsx.Errorf("upload length is larger than rate limit: %d", r.Length))
		}

		if delay := res.Delay(); delay > 0 {
			t.cfg.errors().Printf("c(%p) seed(%t) maximum upload rate exceed n(%d) - delay(%v) - bytes(%d)\n", t.connection, t.t.seeding(), uploaded, delay, r.Length)
			res.Cancel()
			return delay, nil
		}

		more, err := t.sendChunk(r, msg)
		if err != nil {
			t.cfg.errors().Println("error sending chunk to peer, choking peer", err)
			// If we failed to send a chunk, choke the peer to ensure they
			// flush all their requests. We've probably dropped a piece,
			// but there's no way to communicate this to the peer. If they
			// ask for it again, we'll kick them to allow us to send them
			// an updated bitfield.
			t.Choke(msg)
			return 0, nil
		}

		uploaded++
		t.cmu().Lock()
		delete(t.PeerRequests, r)
		t.cmu().Unlock()

		if !more {
			t.cfg.debug().Printf("(%d) c(%p) seed(%t) upload - %d\n", os.Getpid(), t.connection, t.t.seeding(), uploaded)
			return 0, nil
		}
	}

	t.cfg.debug().Printf("(%d) c(%p) seed(%t) upload - %d\n", os.Getpid(), t.connection, t.t.seeding(), uploaded)

	return 0, nil
}

func (t _connwriterRequests) resetuploadavailability(delay time.Duration) {
	if delay == 0 {
		return
	}

	next := time.Now().Add(delay)
	t.uploadavailable.Store(langx.Autoptr(next))
}

func (t _connwriterRequests) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate

	writer := func(msg pp.Message) bool {
		if _, err := ws.Write(msg.MustMarshalBinary()); err != nil {
			ws.cfg.errors().Println(err)
			return false
		}

		ws.wroteMsg(&msg)
		return ws.writeBuffer.Len() < ws.bufferLimit
	}

	if err := ws.checkFailures(); err != nil {
		return cstate.Failure(err)
	}

	available := t.determineInterest(writer)
	if d, err := t.upload(writer); err != nil {
		return cstate.Failure(err)
	} else {
		t.resetuploadavailability(d)
	}

	t.genrequests(available, writer)

	// needresponse is tracking read that come in while we're in the critical section of this function
	// to prevent the state machine from going idle just because we didnt write anything this cycle.
	// needresponse tracks that a message can in that requires a message be sent.
	if ws.writeBuffer.Len() > 0 {
		return connwriterFlush(
			connwriteractive(ws),
			ws,
		)
	}

	return connwriterKeepalive(ws, connwriteridle(ws))
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

	if time.Since(langx.Autoderef(ws.keepaliverequired.Load())) < ws.keepAliveTimeout {
		return connwriterFlush(t.next, ws)
	}

	if _, err = ws.Post(pp.NewKeepAlive()); err != nil {
		return cstate.Failure(errorsx.Wrap(err, "keepalive encoding failed"))
	}

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
	n, err := ws.Flush()

	if err != nil {
		return cstate.Failure(errorsx.Wrap(err, "failed to send requests"))
	}

	if n != 0 {
		ws.keepaliverequired.Store(langx.Autoptr(time.Now().Add(ws.keepAliveTimeout)))
	}

	return t.next
}

func connwriterBitmap(n cstate.T, ws *writerstate) cstate.T {
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

	responseneeded := ws.needsresponse.CompareAndSwap(true, false)
	keepalive := now.Add(ws.keepAliveTimeout / 2)
	if responseneeded {
		keepalive = now
	}

	delays := slicesx.MapTransform(
		func(ts time.Time) time.Duration {
			if ts.Before(now) {
				return ws.keepAliveTimeout / 2
			}
			return time.Until(ts)
		},
		keepalive,
		ws.nextbitmap,
		ws.chokeduntil,
		langx.Autoderef(ws.keepaliverequired.Load()),
		langx.Autoderef(ws.refreshavailable.Load()),
		langx.Autoderef(ws.uploadavailable.Load()),
	)

	mind := timex.DurationMin(delays...)
	if mind <= 0 {
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle uploads(%t) downloads(%t) response needed(%t) %s - %s - %v\n", ws.connection, ws.t.seeding(), !ws.Choked, !ws.PeerChoked, responseneeded, ws.t.chunks, mind, delays)
		return connwriteractive(ws)
	}

	ws.cfg.debug().Printf("c(%p) seed(%t) idling uploads(%t) downloads(%t) response needed(%t) %s - %s - %v\n", ws.connection, ws.t.seeding(), !ws.Choked, !ws.PeerChoked, responseneeded, ws.t.chunks, mind, delays)
	return connwriterBitmap(cstate.Idle(connwriteractive(ws), mind, ws.connection.respond, ws.connection.t.chunks.cond), ws)
}

func connwriteractive(ws *writerstate) cstate.T {
	return connwriterclosed(ws, connWriterSyncChunks(ws))
}
