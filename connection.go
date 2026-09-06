package torrent

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/james-lawrence/torrent/bep0006"
	"github.com/james-lawrence/torrent/bep0009"
	"github.com/james-lawrence/torrent/dht/int160"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/james-lawrence/torrent/bencode"
	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/multiless"
	"github.com/james-lawrence/torrent/internal/timex"
	"github.com/james-lawrence/torrent/mse"
)

type peerSource string

const (
	peerSourceTracker         = "Tr"
	peerSourceIncoming        = "I"
	peerSourceDhtGetPeers     = "Hg" // Peers we found by searching a DHT.
	peerSourceDhtAnnouncePeer = "Ha" // Peers that were announced to us by a DHT.
	peerSourcePex             = "X"
	writebufferscapacity      = 512 * bytesx.KiB
)

func newConnection(cfg *ClientConfig, nc net.Conn, outgoing bool, remote netip.AddrPort, extensions *pp.ExtensionBits, localport uint16, connaddr netip.AddrPort) (c *connection) {
	_mu := &sync.RWMutex{}

	ts := time.Now()
	return &connection{
		_mu:                     _mu,
		upload:                  sync.NewCond(_mu),
		request:                 sync.NewCond(_mu),
		conn:                    nc,
		outgoing:                outgoing,
		Choked:                  atomicx.Bool(true),
		remoteAddr:              remote,
		localport:               localport,
		connaddr:                connaddr,
		peerfastset:             roaring.NewBitmap(),
		claimed:                 roaring.NewBitmap(),
		sentHaves:               roaring.NewBitmap(),
		PeerRequests:            make(map[request]struct{}, cfg.maximumOutstandingRequests),
		PeerExtensionIDs:        make(map[pp.ExtensionName]pp.ExtensionNumber),
		refreshrequestable:      atomicx.Pointer(timex.Inf()),
		lastMessageReceived:     atomicx.Pointer(ts),
		lastRejectReceived:      atomicx.Pointer(ts),
		lastUsefulChunkReceived: atomicx.Pointer(ts),
		extensions:              extensions,
		cfg:                     cfg,
		r:                       nc,
		w:                       nc,
	}
}

// Maintains the state of a connection with a peer.
type connection struct {
	// First to ensure 64-bit alignment for atomics. See #262.
	stats ConnStats

	localport uint16
	connaddr  netip.AddrPort
	dynamicid int160.T

	t *torrent

	_mu *sync.RWMutex

	// The actual Conn, used for closing, and setting socket options.
	conn net.Conn

	outgoing   bool
	network    string
	remoteAddr netip.AddrPort
	// The Reader and Writer for this Conn, with hooks installed for stats,
	// limiting, deadlines etc.
	w io.Writer
	r io.Reader

	// True if the connection is operating over MSE obfuscation.
	headerEncrypted bool
	cryptoMethod    mse.CryptoMethod
	Discovery       peerSource
	trusted         bool
	closed          atomic.Bool

	// Set true after we've added our ConnStats generated during handshake to
	// other ConnStat instances as determined when the *torrent became known.
	reconciledHandshakeStats bool

	// track whenever AllowFast, BitField, Have messages have been received since the last cycle. which allows us to properly single changes.
	refreshrequestable  *atomic.Pointer[time.Time]
	lastMessageReceived *atomic.Pointer[time.Time]
	lastRejectReceived  *atomic.Pointer[time.Time]

	chunksRejected     atomic.Int32
	chunksReceived     atomic.Int32
	completedHandshake time.Time
	// lastUsefulChunkReceived is written from mainReadLoop and read from the
	// writer goroutine plus torrent-level connection ranking (worst_conns.go)
	// - time.Time is a multi-field struct, so an unsynchronized write racing
	// a read here is a real torn-read hazard, not just a detector nicety.
	lastUsefulChunkReceived *atomic.Pointer[time.Time]
	lastChunkSent           time.Time

	// Stuff controlled by the local peer.
	Interested           bool
	lastBecameInterested time.Time
	priorInterest        time.Duration

	Choked *atomic.Bool // we have prevented the peer from making requests
	// requests (the requests we've made of the peer) lives on *writerstate,
	// guarded by its own mutex (writerstate.mu / mutate / view) - mainReadLoop
	// writes to it via ws.mutate, and reads its length via ws.requestsLen.

	// sentHaves is exclusively writer-owned: set once during the pre-spawn
	// handshake (ConnExtensions -> connexfast, which runs synchronously
	// before the reader/writer goroutines are spawned, so that initial write
	// happens-before either goroutine starts) and touched only by the writer
	// goroutine (_connWriterSyncBitfield.Update) after that. Never read or
	// written by the reader or mainReadLoop goroutines - no lock needed.
	sentHaves *roaring.Bitmap

	// local information
	extensions *pp.ExtensionBits
	cfg        *ClientConfig

	// Stuff controlled by the remote peer.
	PeerID                int160.T
	PeerInterested        atomic.Bool
	PeerRequests          map[request]struct{}
	PeerExtensionBytes    pp.ExtensionBits
	PeerPrefersEncryption bool // as indicated by 'e' field in extension handshake
	// The peer's real BitTorrent listening port, as indicated by the 'p'
	// field in its extension handshake. Only meaningful for incoming
	// connections: remoteAddr's port there is the peer's ephemeral outgoing
	// source port for this socket, not their listening port. Zero if the
	// peer hasn't told us (e.g. no extended handshake yet, or it declined).
	PeerListenPort uint16

	// bitmaps representing availability of chunks from the peer.
	claimed     *roaring.Bitmap // represents chunks which our peer claims to have available.
	peerfastset *roaring.Bitmap // represents chunks which we allow our peer to request while choked.
	// fastset (chunks our peer will allow us to request while choked) and
	// touched (pieces we've accepted chunks for from the peer) live on
	// *writerstate, guarded by writerstate.mu - both are written from
	// mainReadLoop (via ws.mutate) and read by the writer goroutine.

	// The peer has everything. This can occur due to a special message, when
	// we may not even know the number of pieces in the torrent yet. Read
	// from the reader, writer, and mainReadLoop goroutines (and torrent-level
	// code iterating connections), written from mainReadLoop - atomic.Bool
	// rather than a plain bool guarded by cn._mu.
	peerSentHaveAll atomic.Bool

	// The highest possible number of pieces the torrent could have based on
	// communication with the peer. Generally only useful until we have the
	// torrent info.
	peerMinPieces uint64

	PeerExtensionIDs map[pp.ExtensionName]pp.ExtensionNumber
	PeerClientName   string

	upload        *sync.Cond  // used to wake up the connection.reader
	request       *sync.Cond  // used to wake up the connection.writer
	needsresponse atomic.Bool // used to track when responses need to be sent that might be missed by the respond condition.
	chunkwakefreq atomic.Uint32
}

func (cn *connection) requestseq() iter.Seq[request] {
	return func(yield func(request) bool) {
		for {
			var (
				req request
			)

			cn._mu.RLock()
			for req = range cn.PeerRequests {
				break
			}
			n := len(cn.PeerRequests)
			cn._mu.RUnlock()

			if n == 0 {
				return
			}

			if !yield(req) {
				return
			}
		}
	}
}

// Returns true if the connection is over IPv6.
func (cn *connection) ipv6() bool {
	return cn.remoteAddr.Addr().Unmap().Is6()
}

// Returns true the dialer has the lower client peer ID. TODO: Find the
// specification for this.
func (cn *connection) isPreferredDirection() bool {
	return cn.dynamicid.Cmp(cn.PeerID) < 0 == cn.outgoing
}

// Returns whether the left connection should be preferred over the right one,
// considering only their networking properties. If ok is false, we can't
// decide.
func (cn *connection) hasPreferredNetworkOver(r *connection) (left, ok bool) {
	var ml multiless.T
	ml.NextBool(cn.isPreferredDirection(), r.isPreferredDirection())
	ml.NextBool(!cn.utp(), !r.utp())
	ml.NextBool(cn.ipv6(), r.ipv6())
	return ml.FinalOk()
}

func (cn *connection) cmu() sync.Locker {
	return cn._mu
}

// Correct the PeerPieces slice length. Return false if the existing slice is
// invalid, such as by receiving badly sized BITFIELD, or invalid HAVE
// messages.
func (cn *connection) resetclaimed() error {
	if cn.peerSentHaveAll.Load() {
		cn.cmu().Lock()
		cn.t.chunks.fill(cn.claimed, uint64(cn.t.chunks.cmaximum))
		cn.cmu().Unlock()
	} else {
		cn.cmu().Lock()
		cn.claimed.Clear()
		cn.cmu().Unlock()
	}

	fastset := errorsx.Zero(bep0006.AllowedFastSet(cn.remoteAddr.Addr(), cn.t.md.ID, cn.t.chunks.pieces, min(32, cn.t.chunks.pieces)))
	cn.cmu().Lock()
	cn.peerfastset = fastset
	cn.cmu().Unlock()
	cn.peerPiecesChanged()

	return nil
}

// peerFastSetEmpty reports whether the peer has allowed us to fast-track any
// chunks while choked. peerfastset is (re)assigned by resetclaimed, which
// torrent-level code calls across every connection of a torrent whenever new
// metainfo is set - not necessarily from this connection's own goroutines -
// so both the assignment and this read must be locked.
func (cn *connection) peerFastSetEmpty() bool {
	cn.cmu().Lock()
	defer cn.cmu().Unlock()
	return cn.peerfastset.IsEmpty()
}

func (cn *connection) connectionFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.cryptoMethod == mse.CryptoMethodRC4 {
		c('E')
	} else if cn.headerEncrypted {
		c('e')
	}
	ret += string(cn.Discovery)
	if cn.utp() {
		c('U')
	}
	return
}

func (cn *connection) utp() bool {
	return strings.Contains(cn.network, "udp")
}

// Close does not itself release outstanding requests back to the chunks
// pool - requests lives on *writerstate, which Close (callable from any
// goroutine, sometimes before a writer even exists) has no reliance on.
// connwriterinit's own cleanup defer already does this unconditionally
// whenever the writer goroutine exits, which Close triggers via ctx
// cancellation.
func (cn *connection) Close() {
	// cn.cfg.debug().Output(2, fmt.Sprintf("c(%p) seed(%t) Close initiated\n", cn, cn.t.seeding()))
	// defer cn.cfg.debug().Output(2, fmt.Sprintf("c(%p) seed(%t) Close initiated\n", cn, cn.t.seeding()))
	cn.cmu().Lock()
	defer cn.cmu().Unlock()

	if cn.closed.Load() {
		return
	}

	if cn.t != nil {
		cn.t.incrementReceivedConns(cn, -1)
	}

	cn.updateRequests()

	if cn.conn != nil {
		cpstats := cn.stats.Copy()
		cn.conn.Close()
		cn.cfg.ConnectionClosed(cn.t.md.ID, cpstats, cn.t.conns.length()-1)
	}
}

func (cn *connection) PeerHasPiece(piece uint64) bool {
	if cn.peerSentHaveAll.Load() {
		return true
	}

	cn.cmu().Lock()
	defer cn.cmu().Unlock()
	return bitmapx.Contains(cn.claimed, cn.t.chunks.chunks(piece)...)
}

// peerRequestsLen returns the number of outstanding requests the peer has
// made of us. PeerRequests is mutated from multiple goroutines (reader,
// writer, and mainReadLoop), so reading its length must hold the lock same
// as any other access.
func (cn *connection) peerRequestsLen() int {
	cn.cmu().Lock()
	defer cn.cmu().Unlock()
	return len(cn.PeerRequests)
}

func (cn *connection) onPeerSentCancel(r request, ws *writerstate) {
	cn._mu.RLock()
	_, ok := cn.PeerRequests[r]
	cn._mu.RUnlock()

	if !ok {
		metrics.Add("unexpected cancels received", 1)
		return
	}

	if cn.supported(pp.ExtensionBitFast) {
		cn.reject(r, ws)
		return
	}

	cn._mu.Lock()
	defer cn._mu.Unlock()
	delete(cn.PeerRequests, r)
}

// Choke and Unchoke are called from both the reader (upload error path) and
// writer (interest/timeout decisions) goroutines, and Choked is read from
// all three (reader, writer, and mainReadLoop) - hence atomic.Bool rather
// than a plain bool guarded by cn._mu.
func (cn *connection) Choke(msg messageWriter, ws *writerstate) error {
	if !cn.Choked.CompareAndSwap(false, true) {
		return nil
	}

	if err := msg(pp.NewChoked()); err != nil {
		return err
	}

	if cn.supported(pp.ExtensionBitFast) {
		cn.cmu().Lock()
		pending := make([]request, 0, len(cn.PeerRequests))
		for r := range cn.PeerRequests {
			pending = append(pending, r)
		}
		cn.cmu().Unlock()

		for _, r := range pending {
			cn.reject(r, ws)
		}
	} else {
		cn.cmu().Lock()
		cn.PeerRequests = nil
		cn.cmu().Unlock()
	}

	return nil
}

func (cn *connection) Unchoke(msg func(pp.Message) bool) bool {
	if !cn.Choked.CompareAndSwap(true, false) {
		return false
	}

	return !msg(pp.NewUnchoked())
}

func (cn *connection) SetInterested(interested bool, msg func(pp.Message) bool) bool {
	if cn.Interested == interested {
		return cn.Interested
	}

	cn.cfg.debug().Printf("c(%p) seed(%t) interest %t -> %t\n", cn, cn.t.seeding(), cn.Interested, interested)
	cn.Interested = interested

	if interested {
		cn.lastBecameInterested = time.Now()
	} else if !cn.lastBecameInterested.IsZero() {
		cn.priorInterest += time.Since(cn.lastBecameInterested)
	}

	return msg(pp.NewInterested(interested))
}

// The function takes a message to be sent, and returns true if more messages
// are okay.
type messageWriter func(pp.Message) error

func (t messageWriter) Deprecated() func(pp.Message) bool {
	return func(m pp.Message) bool {
		return t(m) == nil
	}
}

func (cn *connection) updateRequests() {
	if !cn.needsresponse.Swap(true) {
		cn.upload.Broadcast()
	}
}

func (cn *connection) peerPiecesChanged() {
	if !cn.t.haveInfo() {
		return
	}

	cn.refreshrequestable.Store(new(time.Now()))
	if cn.needsresponse.CompareAndSwap(false, true) {
		cn.request.Broadcast()
	}
}

func (cn *connection) raisePeerMinPieces(newMin uint64) {
	if newMin > cn.peerMinPieces {
		cn.peerMinPieces = newMin
	}
}

func (cn *connection) peerSentHave(piece uint64) error {
	if cn.t.chunks.pieces == 0 {
		// TODO: write a connection test where we send haves before the metadata.
		cn.cfg.debug().Println("encountered bug with have messages when metadata is unknown, mostly harmless may cause issues on torrent with limited seeders")
		return nil
	}

	if piece >= cn.t.chunks.pieces {
		return errorsx.Errorf("invalid piece %d/%d", piece, cn.t.chunks.pieces)
	}

	if cn.PeerHasPiece(piece) {
		return nil
	}

	cn.raisePeerMinPieces(piece + 1)

	cn.cmu().Lock()
	cn.claimed.AddRange(cn.t.chunks.Range(piece))
	cn.cmu().Unlock()

	return nil
}

func (cn *connection) peerSentBitfield(bf []bool) error {
	cn.peerSentHaveAll.Store(false)
	if len(bf)%8 != 0 {
		return errorsx.Errorf("expected bitfield length(%d) divisible by 8", len(bf))
	}

	// We know that the last byte means that at most the last 7 bits are
	// wasted.
	cn.raisePeerMinPieces(uint64(len(bf) - 7))
	if cn.t.haveInfo() && len(bf) > int(cn.t.chunks.pieces+7) {
		// qbittorrent and transmission close the connection here.
		// I suspect other clients do as well as this would be a great way to fuck with a client.
		// it also makes testing more robust.
		return errorsx.Errorf("received a bitfield larger than the number of pieces - %d / %d - %v", len(bf), cn.t.chunks.pieces, bf)
		// old code for reference
		// Ignore known excess pieces.
		// bf = bf[:cn.t.chunks.pieces]
	}

	for i, have := range bf {
		if !have {
			continue
		}

		cn.raisePeerMinPieces(uint64(i) + 1)
		min, max := cn.t.chunks.Range(uint64(i))
		// cn.cfg.debug().Printf("c(%p) seed(%t) adding to claimed %d %d %d %t\n", cn, cn.t.seeding(), i, min, max, have)
		cn._mu.Lock()
		cn.claimed.AddRange(min, max)
		cn._mu.Unlock()
	}
	cn.peerPiecesChanged()
	return nil
}

func (cn *connection) onPeerSentHaveAll() {
	cn.peerSentHaveAll.Store(true)
	cn.cmu().Lock()
	cn.t.chunks.fill(cn.claimed, uint64(cn.t.chunks.cmaximum))
	cn.cmu().Unlock()
	cn.peerPiecesChanged()
}

func (cn *connection) peerSentHaveNone() error {
	cn.peerSentHaveAll.Store(false)
	cn.cmu().Lock()
	cn.claimed.Clear()
	cn.cmu().Unlock()
	cn.peerPiecesChanged()
	return nil
}

func (cn *connection) extensionEnabled(id pp.ExtensionName) bool {
	cn._mu.RLock()
	defer cn._mu.RUnlock()
	return cn.PeerExtensionIDs[id] != 0 && cn.cfg.extensions[id] != 0
}

func (cn *connection) extension(id pp.ExtensionName) pp.ExtensionNumber {
	cn._mu.RLock()
	defer cn._mu.RUnlock()
	return cn.PeerExtensionIDs[id]
}

// peerListenPort reads PeerListenPort under cn._mu - pex.snapshot calls this
// for *other* connections from its own mainReadLoop, not just this
// connection's own goroutines, so the plain field is not safe to read directly.
func (cn *connection) peerListenPort() uint16 {
	cn._mu.RLock()
	defer cn._mu.RUnlock()
	return cn.PeerListenPort
}

func (cn *connection) wroteMsg(msg *pp.Message) {
	cn.allStats(func(cs *ConnStats) { cs.wroteMsg(msg) })
}

func (cn *connection) readMsg(msg *pp.Message) {
	cn.allStats(func(cs *ConnStats) { cs.readMsg(msg) })
}

// After handshake, we know what Torrent and Client stats to include for a
// connection.
func (cn *connection) postHandshakeStats(f func(*ConnStats)) {
	t := cn.t
	f(&t.stats)
	f(&t.cln.stats)
}

// All ConnStats that include this connection. Some objects are not known
// until the handshake is complete, after which it's expected to reconcile the
// differences.
func (cn *connection) allStats(f func(*ConnStats)) {
	f(&cn.stats)
	if cn.reconciledHandshakeStats {
		cn.postHandshakeStats(f)
	}
}

func (cn *connection) wroteBytes(n int64) {
	cn.allStats(add(n, func(cs *ConnStats) *count { return &cs.BytesWritten }))
}

func (cn *connection) readBytes(n int64) {
	cn.allStats(add(n, func(cs *ConnStats) *count { return &cs.BytesRead }))
}

// Returns whether the connection could be useful to us. We're seeding and
// they want data, we don't have metainfo and they can provide it, etc.
func (cn *connection) useful() bool {
	t := cn.t
	if cn.closed.Load() {
		return false
	}

	if !t.haveInfo() {
		return cn.extensionEnabled(pp.ExtensionNameMetadata)
	}

	if t.seeding() && cn.PeerInterested.Load() {
		return true
	}

	return cn.peerHasWantedPieces()
}

func (cn *connection) lastHelpful() (ret time.Time) {
	ret = *cn.lastUsefulChunkReceived.Load()
	if cn.t.seeding() && cn.lastChunkSent.After(ret) {
		ret = cn.lastChunkSent
	}
	return
}

func (cn *connection) supported(b ...uint) bool {
	return cn.extensions.Supported(cn.PeerExtensionBytes, b...)
}

func (cn *connection) reject(r request, ws *writerstate) bool {
	if !cn.supported(pp.ExtensionBitFast) {
		panic("fast not enabled")
	}

	cn.cmu().Lock()
	contains := cn.peerfastset.Contains(r.Index.Uint32())
	cn.cmu().Unlock()
	if contains {
		return false
	}

	ws.Post(r.ToMsg(pp.Reject))

	cn._mu.Lock()
	defer cn._mu.Unlock()
	delete(cn.PeerRequests, r)

	return true
}

func (cn *connection) onReadRequest(r request, ws *writerstate) error {
	requestedChunkLengths.Add(strconv.FormatUint(r.Length.Uint64(), 10), 1)
	cn._mu.RLock()
	_, ok := cn.PeerRequests[r]
	cn._mu.RUnlock()

	if ok {
		metrics.Add("duplicate requests received", 1)
		return nil
	}

	if cn.Choked.Load() {
		if cn.supported(pp.ExtensionBitFast) && cn.reject(r, ws) {
			cn.cmu().Lock()
			fastset := cn.peerfastset.ToArray()
			cn.cmu().Unlock()
			cn.cfg.debug().Printf("c(%p) - rejecting request: choked, cid(%d) %v rejecting request\n", cn, cn.t.chunks.requestCID(r), fastset)
		}

		return nil
	}

	if pending := cn.peerRequestsLen(); !cn.t.seeding() || pending > ws.PendingMaxRequests+maxRequestsGrace {
		if cn.supported(pp.ExtensionBitFast) {
			cn.cfg.debug().Printf("%p - onReadRequest: PeerRequests(%d) > maxRequests(%d), rejecting request\n", cn, pending, ws.PendingMaxRequests)
			cn.reject(r, ws)
		}
		// BEP 6 says we may close here if we choose.
		return nil
	}

	if !cn.t.chunks.ChunksReadable(uint64(r.Index)) {
		// This isn't necessarily them screwing up. We can drop pieces
		// from our storage, and can't communicate this to peers
		// except by reconnecting.
		cn.cfg.debug().Printf("c(%p) - onReadRequest: piece not available %d\n", cn, r.Index)
		return fmt.Errorf("peer requested piece we don't have: %v", r.Index.Int())
	}

	// Check this after we know we have the piece, so that the piece length will be known.
	if r.Begin+r.Length > cn.t.pieceLength(uint64(r.Index)) {
		// log.Printf("%p onReadRequest - request has invalid length: %d received (%d+%d), expected (%d)", cn, r.Index, r.Begin, r.Length, cn.t.pieceLength(uint64(r.Index)))
		return errorsx.New("bad request")
	}

	cn.cmu().Lock()
	cn.PeerRequests[r] = struct{}{}
	cn.cmu().Unlock()

	return nil
}

func (cn *connection) FlushBuffer(buf []byte) (int, error) {
	n, err := cn.w.Write(buf)
	blen := len(buf)

	if err != nil {
		return n, errorsx.Wrap(err, "failed to flush buffer")
	}

	if n != blen {
		return n, errorsx.Errorf("write failed written != len(buf) (%d != %d)", n, blen)
	}

	return n, nil
}

func (cn *connection) ReadOne(ctx context.Context, decoder *pp.Decoder, ws *writerstate) (msg pp.Message, err error) {
	if err = decoder.Decode(&msg); err != nil {
		return msg, err
	}

	cn.readMsg(&msg)
	cn.lastMessageReceived.Store(new(time.Now()))

	if msg.Keepalive {
		dc := cn.t.chunks.Read(copDebugSnapshot)
		cn.cfg.debug().Printf("(%d) c(%p) id(%s) seed(%t) remote(%s) claimed(%d) - RECEIVED KEEPALIVE - missing(%d) - failed(%d) - outstanding(%d) - unverified(%d) - completed(%d)\n", os.Getpid(), cn, cn.t.md.ID, cn.cfg.Seed, cn.conn.RemoteAddr(), cn.claimed.GetCardinality(), dc.missing, dc.failed, dc.outstanding, dc.unverified, dc.completed)
		return
	}

	if msg.Type.FastExtension() && !cn.supported(pp.ExtensionBitFast) {
		return msg, fmt.Errorf("received fast extension message (type=%v) but extension is disabled", msg.Type)
	}

	// runs for every message received, and its arguments are evaluated whether
	// or not debug logging is enabled - a chunks snapshot and two bitmap
	// cardinalities, each behind a lock. uncomment when tracing a connection.
	// dc := cn.t.chunks.Read(copDebugSnapshot)
	// cn.cfg.debug().Printf("(%d) c(%p) id(%s) seed(%t) remote(%s) claimed(%d) - RECEIVED MESSAGE: %s - pending(%d) - missing(%d) - failed(%d) - outstanding(%d) - unverified(%d) - completed(%d)\n", os.Getpid(), cn, cn.t.md.ID, cn.cfg.Seed, cn.conn.RemoteAddr(), cn.claimed.GetCardinality(), msg.Type, ws.requestsLen(), dc.missing, dc.failed, dc.outstanding, dc.unverified, dc.completed)

	switch msg.Type {
	case pp.Choke:
		ws.mutate(func(ws *writerstate) {
			ws.PeerChoked = true
			ws.deleteAllRequestsLocked()
		})
		// We can then reset our interest.
		return msg, nil
	case pp.Unchoke:
		ws.mutate(func(ws *writerstate) { ws.PeerChoked = false })
		cn.peerPiecesChanged()
		return msg, nil
	case pp.Interested:
		cn.PeerInterested.Store(true)
		return msg, nil
	case pp.NotInterested:
		cn.PeerInterested.Store(false)
		// We don't clear their requests since it isn't clear in the spec.
		// We'll probably choke them for this, which will clear them if
		// appropriate, and is clearly specified.
		return msg, nil
	case pp.Have:
		if err = cn.peerSentHave(uint64(msg.Index)); err != nil {
			return msg, err
		}

		cn.peerPiecesChanged()

		return msg, nil
	case pp.Bitfield:

		if err = cn.peerSentBitfield(msg.Bitfield); err != nil {
			return msg, err
		}

		return msg, nil
	case pp.Request:
		if err = cn.onReadRequest(newRequestFromMessage(&msg), ws); err != nil {
			return msg, err
		}
		cn.updateRequests()
		return msg, nil
	case pp.Piece:
		if err = errorsx.Wrap(cn.receiveChunk(&msg, ws), "failed to received chunk"); err != nil {
			return msg, err
		}

		cn.t.chunks.pool.Put(&msg.Piece)

		if n, mod := cn.chunkwakefreq.Add(1), max(ws.PeerMaxRequests.Load()/4, 1); n%mod == 0 {
			cn.request.Broadcast()
		}
		return msg, nil
	case pp.Cancel:
		req := newRequestFromMessage(&msg)
		cn.onPeerSentCancel(req, ws)
		return msg, err
	case pp.Port:
		pingAddr := net.UDPAddr{
			IP:   cn.remoteAddr.Addr().AsSlice(),
			Port: int(cn.remoteAddr.Port()),
		}

		if msg.Port != 0 {
			pingAddr.Port = int(msg.Port)
		}

		cn.t.ping(pingAddr)
		return msg, nil
	case pp.Suggest:
		// cn.cfg.debug().Println("peer suggested piece", msg.Index)
		return msg, nil
	case pp.HaveAll:
		// cn.cfg.debug().Println("peer claims it has everything")
		cn.onPeerSentHaveAll()
		return msg, nil
	case pp.HaveNone:
		// cn.cfg.debug().Println("peer claims it has nothing")
		if err = cn.peerSentHaveNone(); err != nil {
			return msg, err
		}

		return msg, nil
	case pp.Reject:
		if !cn.supported(pp.ExtensionBitFast) {
			return msg, fmt.Errorf("reject recevied, fast not enabled")
		}

		req := newRequestFromMessage(&msg)
		cn.chunksRejected.Add(1)
		ws.mutate(func(ws *writerstate) { ws.clearRequestsLocked(req) })
		return msg, nil
	case pp.AllowedFast:
		min, max := cn.t.chunks.Range(uint64(msg.Index))
		ws.mutate(func(ws *writerstate) { ws.fastset.AddRange(min, max) })

		cn.peerPiecesChanged()

		return msg, nil
	case pp.Extended:
		defer cn.request.Broadcast()
		defer cn.upload.Broadcast()

		if err = cn.onReadExtendedMsg(msg.ExtendedID, msg.ExtendedPayload, ws); err != nil {
			return msg, err
		}
		return msg, nil
	default:
		return msg, errorsx.Errorf("received unknown message type: %#v", msg.Type)
	}
}

// Processes incoming BitTorrent wire-protocol messages. The client lock is held upon entry and
// exit. Returning will end the connection.
func (cn *connection) mainReadLoop(ctx context.Context, ws *writerstate) (err error) {
	cn.cfg.debug().Printf("c(%p) seed(%t) - read loop initiated\n", cn, cn.t.seeding())
	defer cn.cfg.debug().Printf("c(%p) seed(%t) - read loop completed\n", cn, cn.t.seeding())
	defer cn.updateRequests() // tap the writer so it'll clean itself up.

	decoder := pp.NewDecoder(cn.r, cn.t.chunks.pool)

	for {
		_, err := cn.ReadOne(ctx, decoder, ws)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}

	}
}

func (cn *connection) onReadExtendedMsg(id pp.ExtensionNumber, payload []byte, ws *writerstate) (err error) {
	switch id {
	case pp.HandshakeExtendedID:
		var d pp.ExtendedHandshakeMessage
		if err := bencode.Unmarshal(payload, &d); err != nil {
			cn.cfg.errors().Printf("c(%p) seed(%t) error parsing extended handshake message %d %q: %s\n", cn, cn.t.seeding(), id, payload, err)
			return errorsx.Wrap(err, "unmarshalling extended handshake payload")
		}

		ws.PeerMaxRequests.Store(langx.FirstNonZero(uint32(d.Reqq), ws.PeerMaxRequests.Load()))
		cn.PeerClientName = d.V
		// PeerExtensionIDs/PeerPrefersEncryption/PeerListenPort are read under
		// cn._mu by extensionEnabled/extension/pexPeerFlags/peerListenPort -
		// those are called cross-goroutine (the writer's reactive
		// requestPendingMetadata, and pex.snapshot reading *other*
		// connections' fields from their own mainReadLoop), so the writes
		// need the same lock.
		cn._mu.Lock()
		cn.PeerPrefersEncryption = d.Encryption
		cn.PeerExtensionIDs = d.M
		cn.PeerListenPort = uint16(d.Port)
		cn._mu.Unlock()
		cn.cfg.debug().Printf("c(%p) seed(%t) extensions: %s\n", cn, cn.t.seeding(), spew.Sdump(d))

		if d.MetadataSize != 0 {
			if err = cn.t.setMetadataSize(d.MetadataSize); err != nil {
				return errorsx.Wrapf(err, "setting metadata size to %d", d.MetadataSize)
			}

			// setMetadataSize itself only touches torrent-owned state now - it
			// doesn't reach into any connection's writer. Every writer's Idler
			// already treats t.chunks.cond as a signal (connwriterinit), so
			// broadcasting it wakes every peer's writer to reactively pick up
			// pending pieces via writerstate.requestPendingMetadata on its
			// next cycle.
			cn.t.chunks.cond.Broadcast()
		}

		cn.sendInitialPEX(ws)

		// BUG no sending PEX updates yet
		return nil
	case pp.MetadataExtendedID:
		// log.Println("metadata extension available")
		return errorsx.Wrap(cn.t.gotMetadataExtensionMsg(payload, cn, ws), "handling metadata extension message")
	case pp.PEXExtendedID:
		if _, ok := cn.cfg.extensions[pp.ExtensionNamePex]; !ok {
			// TODO: Maybe close the connection.
			return nil
		}

		var pexMsg pp.PexMsg
		err := bencode.Unmarshal(payload, &pexMsg)
		if err != nil {
			return errorsx.Errorf("error unmarshalling PEX message: %s", err)
		}
		metrics.Add("pex added6 peers received", int64(len(pexMsg.Added6)))

		var peers Peers
		peers.AppendFromPex(pexMsg.Added6, pexMsg.Added6Flags)
		peers.AppendFromPex(pexMsg.Added, pexMsg.AddedFlags)
		cn.t.addPeersLocked(peers)
		return nil
	default:
		return errorsx.Errorf("unexpected extended message ID: %v", id)
	}
}

// Process incoming ut_metadata message.
func (t *torrent) gotMetadataExtensionMsg(payload []byte, c *connection, ws *writerstate) error {
	var d bep0009.MetadataResponse
	err := bencode.Unmarshal(payload, &d)
	if _, ok := err.(bencode.ErrUnusedTrailingBytes); ok {
	} else if err != nil {
		return fmt.Errorf("error unmarshalling bencode: %s", err)
	}

	switch d.Type {
	case pp.RequestMetadataExtensionMsgType:
		// log.Printf("c(%p) seed(%t) SENDING METADATA %s\n", c, t.seeding(), spew.Sdump(d))
		if !t.haveMetadataPiece(d.Index) {
			ws.Post(t.newMetadataExtensionMessage(c, pp.RejectMetadataExtensionMsgType, d.Index, nil))
			return nil
		}
		start := 16 * bytesx.KiB * d.Index
		end := start + t.metadataPieceSize(d.Index)
		ws.Post(t.newMetadataExtensionMessage(c, pp.DataMetadataExtensionMsgType, d.Index, t.metadataBytes[start:end]))
		return nil
	case pp.DataMetadataExtensionMsgType:
		// log.Printf("c(%p) seed(%t) RECEIVED METADATA %s\n", c, t.seeding(), spew.Sdump(d))
		c.allStats(add(1, func(cs *ConnStats) *count { return &cs.MetadataChunksRead }))
		if !ws.requestedMetadataPiece(d.Index) {
			return fmt.Errorf("got unexpected piece %d", d.Index)
		}
		ws.mutate(func(ws *writerstate) { ws.metadataRequests[d.Index] = false })
		begin := len(payload) - metadataPieceSize(d.Total, d.Index)
		if begin < 0 || begin >= len(payload) {
			return fmt.Errorf("data has bad offset in payload: %d", begin)
		}

		// log.Printf("c(%p) seed(%t) METADATA SAVE INITIATED %s\n", c, t.seeding(), spew.Sdump(d))
		// defer log.Printf("c(%p) seed(%t) METADATA SAVED %s\n", c, t.seeding(), spew.Sdump(d))

		t.saveMetadataPiece(d.Index, payload[begin:])
		c.lastUsefulChunkReceived.Store(new(time.Now()))
		return t.maybeCompleteMetadata(c)
	case pp.RejectMetadataExtensionMsgType:
		return nil
	default:
		return errorsx.New("unknown msg_type value")
	}
}

// Set both the Reader and Writer for the connection from a single ReadWriter.
func (cn *connection) setRW(rw io.ReadWriter) {
	cn.r = rw
	cn.w = rw
}

// Returns the Reader and Writer as a combined ReadWriter.
func (cn *connection) rw() io.ReadWriter {
	return struct {
		io.Reader
		io.Writer
	}{cn.r, cn.w}
}

// Handle a received chunk from a peer.
func (cn *connection) receiveChunk(msg *pp.Message, ws *writerstate) error {
	req := newRequestFromMessage(msg)

	ws.mutate(func(ws *writerstate) { ws.clearRequestsLocked(req) })

	// Do we actually want this chunk? if the chunk is already available, then we
	// don't need it.
	if cn.t.chunks.Available(req) {
		cn.t.chunks.Release(req)
		cn.cfg.debug().Printf("c(%p) - wasted chunk d(%020d) r(%d,%d,%d)\n", cn, req.Digest, req.Index, req.Begin, req.Length)
		cn.allStats(add(1, func(cs *ConnStats) *count { return &cs.ChunksReadWasted }))
		return nil
	}

	cn.allStats(add(1, func(cs *ConnStats) *count { return &cs.ChunksReadUseful }))
	cn.allStats(add(int64(len(msg.Piece)), func(cs *ConnStats) *count { return &cs.BytesReadUsefulData }))
	cn.lastUsefulChunkReceived.Store(new(time.Now()))
	cn.chunksReceived.Add(1)

	// cn.cfg.debug().Printf("c(%p) - received chunk d(%020d) r(%d,%d,%d)\n", cn, req.Digest, req.Index, req.Begin, req.Length)

	if err := cn.t.writeChunk(int(msg.Index), int64(msg.Begin), msg.Piece); err != nil {
		return errorsx.Wrap(err, "failed to write chunk")
	}

	if err := cn.t.chunks.Verify(req); err != nil {
		return errorsx.Wrap(err, "failed to verify")
	}

	// It's important that the piece is potentially queued before we check if
	// the piece is still wanted, because if it is queued, it won't be wanted.
	if idx := uint64(req.Index); cn.t.chunks.ChunksAvailable(idx) {
		cn.t.digests.Enqueue(idx)
	}

	cid := cn.t.chunks.requestCID(req)
	ws.mutate(func(ws *writerstate) { ws.touched.AddInt(cid) })

	return nil
}

func (cn *connection) peerHasWantedPieces() bool {
	cn._mu.RLock()
	defer cn._mu.RUnlock()
	if cn.claimed.IsEmpty() {
		return false
	}

	return cn.t.chunks.Intersects(cn.claimed, cn.t.chunks.missing)
}

func (cn *connection) setTorrent(t *torrent) {
	if cn.t != nil {
		cn.cfg.errors().Println("BUG: connection already associated with a torrent")
		go cn.Close()
	}
	cn.t = t

	t.incrementReceivedConns(cn, 1)
	t.reconcileHandshakeStats(cn)
}

func (cn *connection) peerPriority() peerPriority {
	return bep40PriorityIgnoreError(cn.remoteAddr, cn.t.cln.publicAddr(cn.remoteAddr))
}

func (cn *connection) String() string {
	return fmt.Sprintf("connection %p", cn)
}

func (cn *connection) pexPeerFlags() pp.PexPeerFlags {
	cn._mu.RLock()
	preferEncryption := cn.PeerPrefersEncryption
	cn._mu.RUnlock()

	f := pp.PexPeerFlags(0)
	if preferEncryption {
		f |= pp.PexPrefersEncryption
	}
	if cn.outgoing {
		f |= pp.PexOutgoingConn
	}
	return f
}

func (cn *connection) sendInitialPEX(ws *writerstate) {
	if !cn.extensionEnabled(pp.ExtensionNamePex) {
		// peer did not advertise support for the PEX extension
		cn.cfg.debug().Printf("pex not supported peer extension enabled(%t) local extension enabled(%t)", cn.extension(pp.ExtensionNamePex) != 0, cn.cfg.extension(pp.ExtensionNamePex) != 0)
		return
	}

	m := cn.t.pex.snapshot(cn)
	if m == nil {
		cn.cfg.debug().Println("pex not enough peers")
		// not enough peers to share — e.g. len(t.conns < 50)
		return
	}

	ws.Post(pp.NewExtended(cn.extension(pp.ExtensionNamePex), bencode.MustMarshal(m)))
}
