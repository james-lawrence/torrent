package torrent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/cstate"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"github.com/james-lawrence/torrent/internal/timex"
)

func connreaderinit(ctx context.Context, cn *connection, to time.Duration) (err error) {
	cn.cfg.debug().Printf("c(%p) reader initiated\n", cn)
	defer cn.cfg.debug().Printf("c(%p) reader completed\n", cn)
	ctx, done := context.WithCancel(ctx)
	defer done()

	ts := time.Now()
	ws := &readerstate{
		bufferLimit:      256 * bytesx.KiB,
		connection:       cn,
		keepAliveTimeout: to,
		chokeduntil:      ts.Add(-1 * time.Minute),
		uploadavailable:  atomicx.Pointer(ts),
		seed:             cn.t.seeding(),
		Idler:            cstate.Idle(ctx, cn.upload, cn.t.chunks.cond),
		requestbuffer:    new(bytes.Buffer),
		pool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, defaultChunkSize)
				return &b
			},
		},
	}

	return cstate.Run(ctx, connReaderAllowRequests(ws), cn.cfg.debug())
}

type readerstate struct {
	*connection
	bufferLimit      int
	keepAliveTimeout time.Duration
	chokeduntil      time.Time
	seed             bool
	uploadavailable  *atomic.Pointer[time.Time]
	pool             *sync.Pool    // buffer pool for storing chunks
	requestbuffer    *bytes.Buffer // message buffer
	*cstate.Idler
}

func (t *readerstate) bufmsg(msg btprotocol.Message) (int, error) {
	n, err := btprotocol.Write(t.requestbuffer, msg)
	if err != nil {
		return n, err
	}

	t.wroteMsg(&msg)

	if t.requestbuffer.Len() < t.bufferLimit {
		return n, nil
	}

	return n, errorsx.Errorf("maximum capacity %d < %d", t.requestbuffer.Len(), t.bufferLimit)
}

func (t *readerstate) bufmsgold(msg btprotocol.Message) error {
	_, err := t.bufmsg(msg)
	return err
}

func connReaderAllowRequests(ws *readerstate) cstate.T {
	return _connreaderAllowRequests{readerstate: ws, next: connReaderUpload(ws)}
}

type _connreaderAllowRequests struct {
	*readerstate
	next cstate.T
}

func (t _connreaderAllowRequests) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	if t.seed || t.chokeduntil.After(time.Now()) {
		if t.Unchoke(messageWriter(t.readerstate.bufmsgold).Deprecated()) {
			t.cfg.debug().Printf("c(%p) seed(%t) allowing peer to make requests\n", t.connection, t.seed)
		}
	} else {
		if t.Choke(t.readerstate.bufmsgold) == nil {
			t.cfg.debug().Printf("c(%p) seed(%t) disallowing peer to make requests\n", t.connection, t.seed)
		}
	}

	return t.next
}

func connReaderUpload(ws *readerstate) cstate.T {
	return _connreaderUpload{readerstate: ws}
}

type _connreaderUpload struct {
	*readerstate
}

func (t *_connreaderUpload) writechunk(r request) (ok bool, err error) {
	b := langx.Autoderef(t.pool.Get().(*[]byte))
	if len(b) < r.Length.Int() {
		b = make([]byte, r.Length)
	}

	defer t.pool.Put(&b)

	b2 := b[:r.Length]

	p := t.t.info.Piece(int(r.Index))
	n, err := t.t.readAt(b2, p.Offset()+int64(r.Begin))
	if n != len(b2) {
		return false, err
	} else if err == io.EOF {
		err = nil
	}

	_, err = t.bufmsg(btprotocol.NewPiece(r.Index, r.Begin, b2))
	if err != nil {
		return false, nil
	}

	return true, nil
}

// Also handles choking and unchoking of the remote peer.
func (t _connreaderUpload) upload() (time.Duration, error) {
	if t.Choked && t.chokeduntil.After(time.Now()) {
		t.cfg.debug().Printf("c(%p) seed(%t) choked(%t) peer completed(%t) req(%d) upload restricted - disallowed\n", t.connection, t.seed, t.Choked, t.peerSentHaveAll, len(t.PeerRequests))
		return timex.DurationMax(time.Until(t.chokeduntil), 0), nil
	}

	if t.peerSentHaveAll {
		t.cfg.debug().Printf("c(%p) seed(%t) choked(%t) peer completed(%t) req(%d) upload restricted - has everything\n", t.connection, t.seed, t.Choked, t.peerSentHaveAll, len(t.PeerRequests))
		return time.Minute, nil
	}

	if len(t.PeerRequests) == 0 {
		t.cfg.debug().Printf("c(%p) seed(%t) choked(%t) peer completed(%t) req(%d) upload restricted - no outstanding requests\n", t.connection, t.seed, t.Choked, t.peerSentHaveAll, len(t.PeerRequests))
		return time.Minute, nil
	}

	// t.cfg.debug().Printf("c(%p) seed(%t) req(%d) uploading - allowed\n", t.connection, t.seed, len(t.PeerRequests))
	// defer t.cfg.debug().Printf("c(%p) seed(%t) req(%d) uploading - completed\n", t.connection, t.seed, len(t.PeerRequests))

	// oreqs := len(t.PeerRequests)
	uploaded := 0
	for r := range t.requestseq() {
		res := t.cfg.UploadRateLimiter.ReserveN(time.Now(), int(r.Length))
		if !res.OK() {
			t.cfg.debug().Printf("upload rate limiter burst size < %d\n", r.Length)
			return 0, connections.NewBanned(t.conn, errorsx.Errorf("upload length is larger than rate limit: %d", r.Length))
		}

		if delay := res.Delay(); delay > 0 {
			t.cfg.errors().Printf("c(%p) seed(%t) maximum upload rate exceed n(%d) - delay(%v) - %s req(%d)\n", t.connection, t.seed, uploaded, delay, r, len(t.PeerRequests))
			res.Cancel()
			return delay, nil
		}

		ok, err := t.writechunk(r)
		if err != nil {
			t.cfg.errors().Println("error sending chunk to peer, choking peer", err)
			// If we failed to send a chunk, choke the peer to ensure they
			// flush all their requests. We've probably dropped a piece,
			// but there's no way to communicate this to the peer. If they
			// ask for it again, we'll kick them to allow us to send them
			// an updated bitfield.
			t.Choke(t.bufmsgold)
			return 0, nil
		}

		uploaded++
		t.cmu().Lock()
		delete(t.PeerRequests, r)
		t.cmu().Unlock()

		if ok {
			continue
		}

		// t.cfg.debug().Printf("c(%p) seed(%t) upload completed - uploaded(%d)/req(%d) - remaining(%d)\n", t.connection, t.seed, uploaded, oreqs, len(t.PeerRequests))
		return 0, nil
	}

	// t.cfg.debug().Printf("c(%p) seed(%t) upload completed - uploaded(%d)/req(%d)\n", t.connection, t.seed, uploaded, oreqs)

	if uploaded == 0 {
		return time.Minute, nil
	}

	return 0, nil
}

func (t _connreaderUpload) resetuploadavailability(delay time.Duration) {
	// t.cfg.debug().Printf("c(%p) seed(%t) setting next upload - %s\n", t.connection, t.seed, delay)
	next := time.Now().Add(delay)
	t.uploadavailable.Store(langx.Autoptr(next))
}

func (t _connreaderUpload) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.readerstate

	if d, err := t.upload(); err != nil {
		return cstate.Failure(err)
	} else {
		t.resetuploadavailability(d)
	}

	// needresponse is tracking read that come in while we're in the critical section of this function
	// to prevent the state machine from going idle just because we didnt write anything this cycle.
	// needresponse tracks that a message can in that requires a message be sent.
	if ws.requestbuffer.Len() > 0 {
		return connreaderFlush(
			ws,
			connReaderUpload(ws),
		)
	}

	return connreaderidle(ws)
}

func connreaderFlush(ws *readerstate, n cstate.T) cstate.T {
	return _connreaderFlush{next: n, readerstate: ws}
}

type _connreaderFlush struct {
	next cstate.T
	*readerstate
}

func (t _connreaderFlush) Update(ctx context.Context, _ *cstate.Shared) cstate.T {
	buf := t.requestbuffer.Bytes()

	if _, err := t.FlushBuffer(buf); err != nil {
		return cstate.Failure(errorsx.Wrap(err, "failed to flush buffer requests"))
	}

	t.requestbuffer.Reset()

	return t.next
}

func connreaderclosed(ws *readerstate, next cstate.T) cstate.T {
	return _connreaderClosed{readerstate: ws, next: next}
}

type _connreaderClosed struct {
	*readerstate
	next cstate.T
}

func (t _connreaderClosed) Update(ctx context.Context, _ *cstate.Shared) (r cstate.T) {
	ws := t.readerstate

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

	// if we've choked them and not allowed to fast track any chunks then there is nothing
	// to do.
	if ws.Choked && ws.peerfastset.IsEmpty() {
		return connreaderFlush(ws, connreaderidle(ws))
	}

	return t.next
}

func connreaderidle(ws *readerstate) cstate.T {
	if ws.needsresponse.CompareAndSwap(true, false) {
		return connreaderactive(ws)
	}

	now := time.Now()
	keepalive := now.Add(ws.keepAliveTimeout / 2)

	delays := slicesx.MapTransform(
		time.Until,
		keepalive,
		langx.Autoderef(ws.uploadavailable.Load()),
	)

	mind := timex.DurationMin(delays...)
	if mind <= 0 {
		ws.cfg.debug().Printf("c(%p) seed(%t) skipping idle uploads(%t) pending(%d) - %s - %v\n", ws.connection, ws.t.seeding(), !ws.Choked, len(ws.PeerRequests), mind, delays)
		return connreaderactive(ws)
	}

	ws.cfg.debug().Printf("c(%p) seed(%t) idling uploads(%t) pending(%d) - %s - %v\n", ws.connection, ws.t.seeding(), !ws.Choked, len(ws.PeerRequests), mind, delays)

	return ws.Idler.Idle(connreaderactive(ws), mind)
}

func connreaderactive(ws *readerstate) cstate.T {
	return connreaderclosed(ws, connReaderAllowRequests(ws))
}
