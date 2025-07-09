package torrent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/bep0006"
	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/cstate"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/backoffx"
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

	if err := ConnExtensions(ctx, c); err != nil {
		err = errorsx.LogErr(errorsx.Wrap(err, "error sending configuring connection"))
		cancel(err)
		return err
	}

	go func() {
		err := connwriterinit(ctx, c, 10*time.Second)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		cancel(err)
		c.Close()
	}()

	go func() {
		err := connreaderinit(ctx, c, 10*time.Second)
		err = errorsx.StdlibTimeout(err, retrydelay, syscall.ECONNRESET)
		cancel(err)
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
		if !cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitExtended) {
			return n
		}

		dynamicport := langx.DefaultIfZero(cn.localport, langx.Autoderef(cn.dynamicaddr.Load()).Port())
		defer cn.cfg.debug().Println("extended handshake extension completed", cn.localport, cn.dynamicaddr)

		// TODO: We can figured the port and address out specific to the socket
		// used.
		msg := btprotocol.ExtendedHandshakeMessage{
			M:            cn.cfg.extensions,
			V:            cn.cfg.ExtendedHandshakeClientVersion,
			Reqq:         cn.cfg.maximumOutstandingRequests,
			YourIp:       btprotocol.CompactIp(cn.remoteAddr.Addr().AsSlice()),
			Encryption:   cn.cfg.HeaderObfuscationPolicy.Preferred || cn.cfg.HeaderObfuscationPolicy.RequirePreferred,
			Port:         int(dynamicport),
			MetadataSize: cn.t.metadatalen(),
			Ipv4:         btprotocol.CompactIp(cn.cfg.publicIP4.To4()),
			Ipv6:         cn.cfg.publicIP6.To16(),
		}

		// cn.cfg.debug().Printf("c(%p) seed(%t) extended handshake: %s\n", cn, cn.t.seeding(), spew.Sdump(msg))

		encoded, err := bencode.Marshal(msg)
		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		_, err = cn.Post(btprotocol.NewExtendedHandshake(encoded))

		if err != nil {
			return cstate.Failure(errorsx.Wrapf(err, "unable to encode message %T", msg))
		}

		return n
	})
}

func connexfast(cn *connection, n cstate.T) cstate.T {
	return cstate.Fn(func(context.Context, *cstate.Shared) cstate.T {
		defer cn.cfg.debug().Printf("c(%p) seed(%t) fast extension completed\n", cn, cn.t.seeding())
		if !cn.supported(btprotocol.ExtensionBitFast) {
			cn.sentHaves = cn.t.chunks.CompletedBitmap()
			if _, err := cn.PostBitfield(cn.sentHaves); err != nil {
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
			if _, err := cn.Post(btprotocol.NewHaveNone()); err != nil {
				return cstate.Failure(err)
			}
			cn.sentHaves.Clear()
			return n
		case uint64(cn.t.chunks.cmaximum):
			cn.cfg.debug().Printf("c(%p) seed(%t) posting allow fast have all: %d/%d\n", cn, cn.t.seeding(), readable, cn.t.chunks.cmaximum)
			if _, err := cn.Post(btprotocol.NewHaveAll()); err != nil {
				return cstate.Failure(err)
			}

			cn.sentHaves.AddRange(0, cn.t.chunks.pieces)

			for _, v := range cn.peerfastset.ToArray() {
				if _, err := cn.Post(btprotocol.NewAllowedFast(v)); err != nil {
					return cstate.Failure(err)
				}
			}

			return n
		default:
			cn.cfg.debug().Printf("c(%p) seed(%t) posting bitfield: r(%d) u(%d) c(%d) cmax(%d)\n", cn, cn.t.seeding(), readable, cn.t.chunks.unverified.GetCardinality(), cn.t.chunks.completed.GetCardinality(), cn.t.chunks.cmaximum)
			cn.sentHaves = cn.t.chunks.CompletedBitmap()
			if _, err := cn.PostBitfield(cn.sentHaves); err != nil {
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
		if !(cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitDHT) && port > 0) {
			cn.cfg.debug().Printf("posting dht not supported extension supported(%t) - port(%d)\n", cn.extensions.Supported(cn.PeerExtensionBytes, btprotocol.ExtensionBitDHT), port)
			return n
		}

		defer cn.cfg.debug().Println("dht extension completed")

		_, err := cn.Post(btprotocol.NewPort(port))
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
	ctx, done := context.WithCancel(ctx)
	defer done()

	ts := time.Now()
	ws := &writerstate{
		bufferLimit:         writebufferscapacity,
		connection:          cn,
		keepAliveTimeout:    to,
		chokeduntil:         ts.Add(-1 * time.Minute),
		nextbitmap:          ts.Add(time.Minute),
		keepaliverequired:   atomicx.Pointer(ts.Add(to)),
		resyncbitfield:      atomicx.Pointer(ts.Add(time.Minute)),
		lowrequestwatermark: max(1, cn.PeerMaxRequests/4),
		requestable:         roaring.New(),
		seed:                cn.t.seeding(),
		Idler:               cstate.Idle(ctx, cn.request, cn.t.chunks.cond),
	}

	defer cn.checkFailures()
	defer cn.deleteAllRequests()

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
	*cstate.Idler
}

func (t *writerstate) bufmsg(msg btprotocol.Message) error {
	if _, err := t.Write(msg.MustMarshalBinary()); err != nil {
		return err
	}

	t.wroteMsg(&msg)

	if t.currentbuffer.Len() < t.bufferLimit {
		return nil
	}

	return errorsx.Errorf("maximum capacity %d < %d", t.currentbuffer.Len(), t.bufferLimit)
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
		return len(ws.requests) > 0 && cn.lastUsefulChunkReceived.Add(grace).Before(time.Now()) && cn.t.chunks.Cardinality(cn.t.chunks.missing) > 0
	}

	if ws.closed.Load() {
		return nil
	}

	// if we're choked and not allowed to fast track any chunks then there is nothing
	// to do.
	if ws.PeerChoked && ws.fastset.IsEmpty() {
		return connwriterFlush(t.next, ws)
	}

	// detect effectively dead connections, choking them for at least 1 minute.
	if timedout(ws.connection, ws.t.chunks.gracePeriod) {
		if err := ws.Choke(ws.bufmsg); err != nil {
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

func connWriterInterested(ws *writerstate, next cstate.T) cstate.T {
	return _connWriterInterested{writerstate: ws, next: next}
}

type _connWriterInterested struct {
	*writerstate
	next cstate.T
}

func (t _connWriterInterested) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.writerstate
	next := connwriterRequests(ws)

	if ws.t.chunks.Incomplete() {
		return next
	}

	ws.SetInterested(false, messageWriter(ws.bufmsg).Deprecated())
	return next
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
		if t.Choke(msg) == nil {
			t.cfg.debug().Printf("c(%p) seed(%t) disallowing peer to make requests\n", t.connection, t.seed)
		}
	}

	if ts := langx.Autoderef(t.refreshrequestable.Load()); ts.After(time.Now()) {
		t.cfg.debug().Printf("c(%p) seed(%t) allowing cached %d - %s\n", t.connection, t.seed, t.requestable.GetCardinality(), time.Until(ts))
		return t.requestable
	}

	if m := t.t.chunks.Cardinality(t.t.chunks.completed); uint64(m) == t.t.chunks.pieces {
		t.cfg.debug().Printf("c(%p) seed(%t) disabling requestable - have all data m(%d) o(%d) c(%d) p(%d)\n", t.connection, t.seed, m, len(t.t.chunks.outstanding), t.t.chunks.Cardinality(t.t.chunks.completed), t.t.chunks.pieces)
		t.refreshrequestable.Store(langx.Autoptr(timex.Inf()))
		t.requestable = roaring.New()
		return t.requestable
	}

	t.cfg.debug().Printf("c(%p) seed(%t) refreshing availability\n", t.connection, t.seed)

	t._mu.RLock()
	fastset := t.fastset.Clone()
	claimed := roaring.New()
	if !t.PeerChoked {
		// t.cfg.debug().Printf("c(%p) seed(%t) allowing claimed: %d\n", t.connection, t.seed, t.claimed.GetCardinality())
		claimed = t.claimed.Clone()
	}
	t._mu.RUnlock()

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
	t.cmu().Lock()
	t.requests[r.Digest] = r
	t.cmu().Unlock()

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

	if len(t.requests) > t.lowrequestwatermark/2 {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - req(current(%d) >= low watermark(%d) / 2)", t.connection, t.seed, len(t.requests), t.lowrequestwatermark)
		return
	}

	if unmodified := !t.refreshrequestable.Load().Before(time.Now()); available.IsEmpty() && unmodified {
		t.cfg.debug().Printf("c(%p) seed(%t) skipping buffer fill - avail(%d) && unmodified(%t)", t.connection, t.seed, available.GetCardinality(), unmodified)
		return
	}

	// once we fall below the low watermark dynamic adjust it based on what we saw.
	// never allowing it to go above the original low watermark and with a floor of a single request.
	t.lowrequestwatermark += min(1, int(t.chunksReceived.Swap(0)*4-t.chunksRejected.Swap(0)))
	t.lowrequestwatermark = min(t.lowrequestwatermark, t.PeerMaxRequests-(t.PeerMaxRequests/4))

	max := max(0, t.lowrequestwatermark-len(t.requests))
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

	// t.cfg.debug().Printf("c(%p) seed(%t) avail(%d) filling buffer with requests low(%d) - max(%d) outstanding(%d) -> allowed(%d) actual %d", t.connection, t.seed, available.GetCardinality(), t.lowrequestwatermark, t.PeerMaxRequests, len(t.requests), max, len(reqs))

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

	t.genrequests(t.determineInterest(ws.bufmsg), ws.bufmsg)

	// needresponse is tracking read that come in while we're in the critical section of this function
	// to prevent the state machine from going idle just because we didnt write anything this cycle.
	// needresponse tracks that a message can in that requires a message be sent.
	if ws.currentbuffer.Len() > 0 {
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

	if langx.Autoderef(ws.keepaliverequired.Load()).After(time.Now()) {
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
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle downloads(%t) %s - needs rsponse\n", ws.connection, ws.t.seeding(), !ws.PeerChoked, ws.t.chunks)
		return connwriteractive(ws)
	}

	delays := slicesx.MapTransform(
		time.Until,
		keepalive,
		ws.nextbitmap,
		timex.Max(ws.chokeduntil, keepalive),
		langx.Autoderef(ws.keepaliverequired.Load()),
		langx.Autoderef(ws.refreshrequestable.Load()),
	)

	mind := timex.DurationMin(delays...)
	if mind <= 0 {
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle downloads(%t) avail(%d) %s - %s - %v\n", ws.connection, ws.t.seeding(), !ws.PeerChoked, ws.requestable.GetCardinality(), ws.t.chunks, mind, delays)
		return connwriteractive(ws)
	}

	ws.cfg.debug().Printf("c(%p) seed(%t) idling downloads(%t) avail(%d) %s - %s - %v\n", ws.connection, ws.t.seeding(), !ws.PeerChoked, ws.requestable.GetCardinality(), ws.t.chunks, mind, delays)

	return connWriterSyncBitfield(ws, connWriterInterested(ws, ws.Idler.Idle(connwriteractive(ws), mind)))
}

func connwriteractive(ws *writerstate) cstate.T {
	return connwriterKeepalive(ws, connwriterclosed(ws, connwriterBitmap(ws, connWriterInterested(ws, connwriterRequests(ws)))))
}
