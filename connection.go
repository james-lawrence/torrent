package torrent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/missinggo/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/james-lawrence/torrent/bep0006"
	"github.com/james-lawrence/torrent/bep0009"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/dht/int160"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/james-lawrence/torrent/bencode"
	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/x/bitmapx"
	"github.com/james-lawrence/torrent/mse"
)

type peerSource string

const (
	peerSourceTracker         = "Tr"
	peerSourceIncoming        = "I"
	peerSourceDhtGetPeers     = "Hg" // Peers we found by searching a DHT.
	peerSourceDhtAnnouncePeer = "Ha" // Peers that were announced to us by a DHT.
	peerSourcePex             = "X"
)

func newConnection(cfg *ClientConfig, nc net.Conn, outgoing bool, remote netip.AddrPort, extensions *pp.ExtensionBits, localport uint16, dynamicaddr *atomic.Pointer[netip.AddrPort]) (c *connection) {
	_mu := &sync.RWMutex{}

	ts := time.Now()
	return &connection{
		_mu:                     _mu,
		respond:                 sync.NewCond(_mu),
		conn:                    nc,
		outgoing:                outgoing,
		Choked:                  true,
		PeerChoked:              true,
		PeerMaxRequests:         cfg.maximumOutstandingRequests,
		PendingMaxRequests:      cfg.maximumOutstandingRequests,
		writeBuffer:             new(bytes.Buffer),
		remoteAddr:              remote,
		localport:               localport,
		dynamicaddr:             dynamicaddr,
		touched:                 roaring.NewBitmap(),
		peerfastset:             roaring.NewBitmap(),
		fastset:                 roaring.NewBitmap(),
		claimed:                 roaring.NewBitmap(),
		sentHaves:               roaring.NewBitmap(),
		requests:                make(map[uint64]request, cfg.maximumOutstandingRequests),
		PeerRequests:            make(map[request]struct{}, cfg.maximumOutstandingRequests),
		PeerExtensionIDs:        make(map[pp.ExtensionName]pp.ExtensionNumber),
		refreshrequestable:      atomicx.Pointer(ts),
		lastMessageReceived:     atomicx.Pointer(ts),
		lastRejectReceived:      atomicx.Pointer(ts),
		lastUsefulChunkReceived: ts,
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

	localport   uint16
	dynamicaddr *atomic.Pointer[netip.AddrPort]

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

	chunksRejected          atomic.Int32
	chunksReceived          atomic.Int32
	completedHandshake      time.Time
	lastUsefulChunkReceived time.Time
	lastChunkSent           time.Time

	// Stuff controlled by the local peer.
	Interested           bool
	lastBecameInterested time.Time
	priorInterest        time.Duration

	lastStartedExpectingToReceiveChunks time.Time
	cumulativeExpectedToReceiveChunks   time.Duration
	chunksReceivedWhileExpecting        int64

	Choked           bool // we have preventing the peer from making requests
	requests         map[uint64]request
	requestsLowWater int

	// Indexed by metadata piece, set to true if posted and pending a
	// response.
	metadataRequests []bool
	sentHaves        *roaring.Bitmap

	// local information
	extensions *pp.ExtensionBits
	cfg        *ClientConfig

	// Stuff controlled by the remote peer.
	PeerID                int160.T
	PeerInterested        bool
	PeerChoked            bool // peer has restricted us from making requests.
	PeerRequests          map[request]struct{}
	PeerExtensionBytes    pp.ExtensionBits
	PeerPrefersEncryption bool // as indicated by 'e' field in extension handshake

	// bitmaps representing availability of chunks from the peer.
	claimed     *roaring.Bitmap // represents chunks which our peer claims to have available.
	peerfastset *roaring.Bitmap // represents chunks which we allow our peer to request while choked.
	fastset     *roaring.Bitmap // represents chunks which our peer will allow us to request while choked.
	// pieces we've accepted chunks for from the peer.
	touched *roaring.Bitmap

	// The pieces the peer has claimed to have.
	// peerPieces bitmap.Bitmap

	// The peer has everything. This can occur due to a special message, when
	// we may not even know the number of pieces in the torrent yet.
	peerSentHaveAll bool

	// The highest possible number of pieces the torrent could have based on
	// communication with the peer. Generally only useful until we have the
	// torrent info.
	peerMinPieces uint64

	PeerMaxRequests    int // Maximum pending requests the peer allows.
	PendingMaxRequests int // Maximum pending requests the client allows.
	PeerExtensionIDs   map[pp.ExtensionName]pp.ExtensionNumber
	PeerClientName     string

	writeBuffer   *bytes.Buffer
	respond       *sync.Cond
	needsresponse atomic.Bool // used to track when responses need to be sent that might be missed by the respond condition.
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

func (cn *connection) updateExpectingChunks() {
	if cn.expectingChunks() {
		if cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.lastStartedExpectingToReceiveChunks = time.Now()
		}
	} else {
		if !cn.lastStartedExpectingToReceiveChunks.IsZero() {
			cn.cumulativeExpectedToReceiveChunks += time.Since(cn.lastStartedExpectingToReceiveChunks)
			cn.lastStartedExpectingToReceiveChunks = time.Time{}
		}
	}
}

func (cn *connection) expectingChunks() bool {
	return cn.Interested && !cn.PeerChoked
}

// Returns true if the connection is over IPv6.
func (cn *connection) ipv6() bool {
	return cn.remoteAddr.Addr().Unmap().Is6()
}

// Returns true the dialer has the lower client peer ID. TODO: Find the
// specification for this.
func (cn *connection) isPreferredDirection() bool {
	return cn.cfg.localID.Cmp(cn.PeerID) < 0 == cn.outgoing
}

// Returns whether the left connection should be preferred over the right one,
// considering only their networking properties. If ok is false, we can't
// decide.
func (cn *connection) hasPreferredNetworkOver(r *connection) (left, ok bool) {
	var ml missinggo.MultiLess
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
	if cn.peerSentHaveAll {
		cn.cmu().Lock()
		cn.t.chunks.fill(cn.claimed)
		cn.cmu().Unlock()
	} else {
		cn.cmu().Lock()
		cn.t.chunks.fill(cn.claimed).Clear()
		cn.cmu().Unlock()
	}

	cn.peerfastset = errorsx.Zero(bep0006.AllowedFastSet(cn.remoteAddr.Addr(), cn.t.md.ID, cn.t.chunks.pieces, min(32, cn.t.chunks.pieces)))
	cn.peerPiecesChanged()
	return nil
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

func (cn *connection) Close() {
	// cn.cfg.debug().Output(2, fmt.Sprintf("c(%p) seed(%t) Close initiated\n", cn, cn.t.seeding()))
	// defer cn.cfg.debug().Output(2, fmt.Sprintf("c(%p) seed(%t) Close initiated\n", cn, cn.t.seeding()))
	defer cn.t.cln.event.Broadcast()
	defer cn.deleteAllRequests()
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
	return cn.peerSentHaveAll || bitmapx.Contains(cn.claimed, cn.t.chunks.chunks(piece)...)
}

// Writes a message into the write buffer.
func (cn *connection) Post(msg pp.Message) (n int, err error) {
	// cn.cfg.debug().Output(2, fmt.Sprintf("c(%p) seed(%t) Post initiated: %s\n", cn, cn.t.seeding(), msg.Type))

	encoded, err := msg.MarshalBinary()
	if err != nil {
		return n, errorsx.Wrapf(err, "failed to encode message %T", msg)
	}

	n, err = cn.Write(encoded)
	if err != nil {
		return n, errorsx.Wrapf(err, "failed to write message into buffer %T", msg)
	}

	cn.wroteMsg(&msg)
	cn.updateRequests()
	return n, nil
}

func (cn *connection) Write(encoded []byte) (n int, err error) {
	cn.cmu().Lock()
	defer cn.cmu().Unlock()
	return cn.writeBuffer.Write(encoded)
}

// Writes a message into the write buffer.
func (cn *connection) PostImmediate(msg pp.Message) (n int, err error) {
	if _, err := cn.Post(msg); err != nil {
		return n, err
	}

	return cn.Flush()
}

func (cn *connection) requestMetadataPiece(index int) {
	if index < len(cn.metadataRequests) && cn.metadataRequests[index] {
		return
	}

	encoded, err := bencode.Marshal(bep0009.MetadataRequest{
		Type:  pp.RequestMetadataExtensionMsgType,
		Index: index,
	})
	if err != nil {
		log.Println("able to encoded metadata request", err)
		return
	}

	if _, err := cn.Post(pp.NewExtended(cn.extension(pp.ExtensionNameMetadata), encoded)); err != nil {
		log.Println("able to post metadata request", err)
		return
	}

	for index >= len(cn.metadataRequests) {
		cn.metadataRequests = append(cn.metadataRequests, false)
	}

	cn.metadataRequests[index] = true
}

func (cn *connection) requestedMetadataPiece(index int) bool {
	return index < len(cn.metadataRequests) && cn.metadataRequests[index]
}

func (cn *connection) onPeerSentCancel(r request) {
	cn._mu.RLock()
	_, ok := cn.PeerRequests[r]
	cn._mu.RUnlock()

	if !ok {
		metrics.Add("unexpected cancels received", 1)
		return
	}

	if cn.supported(pp.ExtensionBitFast) {
		cn.reject(r)
		return
	}

	cn._mu.Lock()
	defer cn._mu.Unlock()
	delete(cn.PeerRequests, r)
}

func (cn *connection) Choke(msg messageWriter) (more bool) {
	if cn.Choked {
		return true
	}

	cn.Choked = true
	more = msg(pp.NewChoked())

	if cn.supported(pp.ExtensionBitFast) {
		for r := range cn.PeerRequests {
			cn.reject(r)
		}
	} else {
		cn.PeerRequests = nil
	}

	return more
}

func (cn *connection) Unchoke(msg func(pp.Message) bool) bool {
	if !cn.Choked {
		return false
	}
	cn.Choked = false

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

	defer cn.updateExpectingChunks()

	return msg(pp.NewInterested(interested))
}

// The function takes a message to be sent, and returns true if more messages
// are okay.
type messageWriter func(pp.Message) bool

// connections check their own failures, this amortizes the cost of failures to
// the connections themselves instead of bottlenecking at the torrent.
func (cn *connection) checkFailures() error {
	cn.cmu().Lock()
	defer cn.cmu().Unlock()

	failed := cn.t.chunks.Failed(cn.touched.Clone())

	if failed.IsEmpty() {
		return nil
	}

	for iter, prev, pid := failed.ReverseIterator(), -1, 0; iter.HasNext(); prev = pid {
		pid = cn.t.chunks.pindex(int(iter.Next()))
		if pid == prev {
			continue
		}

		cn.stats.PiecesDirtiedBad.Add(1)
		if !cn.t.chunks.ChunksComplete(uint64(pid)) {
			cn.t.chunks.ChunksRetry(uint64(pid))
		}
	}

	if cn.stats.PiecesDirtiedBad.Int64() > 10 {
		return connections.NewBanned(cn.conn, errorsx.New("too many bad pieces"))
	}

	return nil
}

func (cn *connection) Have(piece uint64) (n int, err error) {
	cn.cmu().Lock()
	added := cn.sentHaves.CheckedAdd(uint32(piece))
	cn.cmu().Unlock()
	if !added {
		return 0, nil
	}

	return cn.Post(pp.NewHavePiece(piece))
}

func (cn *connection) PostBitfield() (n int, err error) {
	dup := cn.t.chunks.CompletedBitmap()

	cn.cfg.debug().Printf("c(%p) seed(%t) calculated bitfield: b(%d)/p(%d)\n", cn, cn.t.seeding(), dup.GetCardinality(), cn.t.chunks.pieces)
	n, err = cn.Post(pp.NewBitField(cn.t.chunks.pieces, dup))
	if err != nil {
		return n, err
	}

	cn.sentHaves = bitmapx.Lazy(dup)
	return n, nil
}

func (cn *connection) updateRequests() {
	if cn.needsresponse.Swap(true) {
		cn.respond.Broadcast()
	}
}

func (cn *connection) peerPiecesChanged() {
	if !cn.t.haveInfo() {
		return
	}

	cn.t.event.Broadcast()
	cn.updateRequests()
}

func (cn *connection) raisePeerMinPieces(newMin uint64) {
	if newMin > cn.peerMinPieces {
		cn.peerMinPieces = newMin
	}
}

func (cn *connection) peerSentHave(piece uint64) error {
	if piece >= cn.t.chunks.pieces {
		return errorsx.New("invalid piece")
	}

	if cn.PeerHasPiece(piece) {
		return nil
	}

	cn.raisePeerMinPieces(piece + 1)

	cn.cmu().Lock()
	for _, cidx := range cn.t.chunks.chunks(piece) {
		cn.claimed.AddInt(cidx)
	}
	cn.cmu().Unlock()

	return nil
}

func (cn *connection) peerSentBitfield(bf []bool) error {
	cn.peerSentHaveAll = false
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

func (cn *connection) onPeerSentHaveAll() error {
	cn.cmu().Lock()
	cn.peerSentHaveAll = true
	cn.t.chunks.fill(cn.claimed)
	cn.cmu().Unlock()
	cn.peerPiecesChanged()
	return nil
}

func (cn *connection) peerSentHaveNone() error {
	cn.cmu().Lock()
	cn.peerSentHaveAll = false
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

func (cn *connection) requestPendingMetadata() {
	if !cn.extensionEnabled(pp.ExtensionNameMetadata) {
		cn.cfg.debug().Println("connection doesnt support metadata")
		return
	}

	if cn.t.haveInfo() {
		cn.cfg.debug().Printf("c(%p) seed(%t) metadata ex: ignoring already have torrent\n", cn, cn.t.seeding())
		return
	}

	cn.cfg.debug().Println("metadata ex: requesting metadata")

	// Request metadata pieces that we don't have in a random order.
	var pending []int
	for index := 0; index < cn.t.metadataPieceCount(); index++ {
		if !cn.t.haveMetadataPiece(index) && !cn.requestedMetadataPiece(index) {
			pending = append(pending, index)
		}
	}

	rand.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	for _, i := range pending {
		cn.requestMetadataPiece(i)
	}
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

	if t.seeding() && cn.PeerInterested {
		return true
	}

	return cn.peerHasWantedPieces()
}

func (cn *connection) lastHelpful() (ret time.Time) {
	ret = cn.lastUsefulChunkReceived
	if cn.t.seeding() && cn.lastChunkSent.After(ret) {
		ret = cn.lastChunkSent
	}
	return
}

func (cn *connection) supported(b ...uint) bool {
	return cn.extensions.Supported(cn.PeerExtensionBytes, b...)
}

func (cn *connection) reject(r request) bool {
	if !cn.supported(pp.ExtensionBitFast) {
		panic("fast not enabled")
	}

	if cn.peerfastset.Contains(r.Index.Uint32()) {
		return false
	}

	cn.Post(r.ToMsg(pp.Reject))

	cn._mu.Lock()
	defer cn._mu.Unlock()
	delete(cn.PeerRequests, r)

	return true
}

func (cn *connection) onReadRequest(r request) error {
	requestedChunkLengths.Add(strconv.FormatUint(r.Length.Uint64(), 10), 1)
	cn._mu.RLock()
	_, ok := cn.PeerRequests[r]
	cn._mu.RUnlock()

	if ok {
		metrics.Add("duplicate requests received", 1)
		return nil
	}

	if cn.Choked {
		if cn.supported(pp.ExtensionBitFast) && cn.reject(r) {
			cn.cfg.debug().Printf("c(%p) - rejecting request: choked, cid(%d) %v rejecting request\n", cn, cn.t.chunks.requestCID(r), cn.peerfastset.ToArray())
		}

		return nil
	}

	if pending := len(cn.PeerRequests); !cn.t.seeding() || pending > cn.PendingMaxRequests+maxRequestsGrace {
		if cn.supported(pp.ExtensionBitFast) {
			cn.cfg.debug().Printf("%p - onReadRequest: PeerRequests(%d) > maxRequests(%d), rejecting request\n", cn, pending, cn.PendingMaxRequests)
			cn.reject(r)
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

func (cn *connection) Flush() (int, error) {
	// olen := cn.writeBuffer.Len()
	// defer func() {
	// 	log.Printf("c(%p) seed(%t) -------------------------------- flushed %d --------------------------------\n", cn, cn.t.seeding(), olen)
	// }()

	cn.cmu().Lock()
	buf := cn.writeBuffer.Bytes()
	cn.writeBuffer.Reset()
	cn.cmu().Unlock()

	n, err := cn.w.Write(buf)

	if err != nil {
		return n, errorsx.Wrap(err, "failed to flush buffer")
	}

	if n != len(buf) {
		return n, errorsx.Errorf("write failed written != len(buf) (%d != %d)", n, len(buf))
	}

	return n, nil
}

func (cn *connection) ReadOne(ctx context.Context, decoder *pp.Decoder) (msg pp.Message, err error) {
	if err = decoder.Decode(&msg); err != nil {
		return msg, err
	}

	cn.readMsg(&msg)
	cn.lastMessageReceived.Store(langx.Autoptr(time.Now()))

	if msg.Keepalive {
		cn.cfg.debug().Printf("(%d) c(%p) seed(%t) - RECEIVED KEEPALIVE - missing(%d) - failed(%d) - outstanding(%d) - unverified(%d) - completed(%d)\n", os.Getpid(), cn, cn.cfg.Seed, cn.t.chunks.Cardinality(cn.t.chunks.missing), cn.t.chunks.Cardinality(cn.t.chunks.failed), len(cn.t.chunks.outstanding), cn.t.chunks.Cardinality(cn.t.chunks.unverified), cn.t.chunks.Cardinality(cn.t.chunks.completed))
		return
	}

	if msg.Type.FastExtension() && !cn.supported(pp.ExtensionBitFast) {
		return msg, fmt.Errorf("received fast extension message (type=%v) but extension is disabled", msg.Type)
	}

	cn.cfg.debug().Printf("(%d) c(%p) seed(%t) remote(%s) - RECEIVED MESSAGE: %s - pending(%d) - missing(%d) - failed(%d) - outstanding(%d) - unverified(%d) - completed(%d)\n", os.Getpid(), cn, cn.cfg.Seed, cn.conn.RemoteAddr(), msg.Type, len(cn.requests), cn.t.chunks.Cardinality(cn.t.chunks.missing), cn.t.chunks.Cardinality(cn.t.chunks.failed), len(cn.t.chunks.outstanding), cn.t.chunks.Cardinality(cn.t.chunks.unverified), cn.t.chunks.Cardinality(cn.t.chunks.completed))

	switch msg.Type {
	case pp.Choke:
		cn.PeerChoked = true
		cn.deleteAllRequests()
		cn.updateExpectingChunks()
		// We can then reset our interest.
		return msg, nil
	case pp.Unchoke:
		cn.PeerChoked = false
		cn.updateExpectingChunks()
		cn.updateRequests()
		return msg, nil
	case pp.Interested:
		cn.PeerInterested = true
		cn.updateRequests()
		return msg, nil
	case pp.NotInterested:
		cn.PeerInterested = false
		cn.updateRequests()
		// We don't clear their requests since it isn't clear in the spec.
		// We'll probably choke them for this, which will clear them if
		// appropriate, and is clearly specified.
		return msg, nil
	case pp.Have:
		if err = cn.peerSentHave(uint64(msg.Index)); err != nil {
			return msg, err
		}

		now := time.Now()
		if ts := cn.refreshrequestable.Swap(langx.Autoptr(now.Add(30 * time.Millisecond))); ts.After(now) {
			cn.updateRequests()
		}

		return msg, nil
	case pp.Bitfield:

		if err = cn.peerSentBitfield(msg.Bitfield); err != nil {
			return msg, err
		}

		now := time.Now()
		if ts := cn.refreshrequestable.Swap(langx.Autoptr(now.Add(30 * time.Millisecond))); ts.After(now) {
			cn.updateRequests()
		}

		return msg, nil
	case pp.Request:
		if err = cn.onReadRequest(newRequestFromMessage(&msg)); err != nil {
			return msg, err
		}
		cn.updateRequests()
		return msg, nil
	case pp.Piece:
		if !cn.needsresponse.Load() {
			defer cn.updateRequests()
		}

		if err = errorsx.Wrap(cn.receiveChunk(&msg), "failed to received chunk"); err != nil {
			return msg, err
		}

		cn.t.chunks.pool.Put(&msg.Piece)

		return msg, nil
	case pp.Cancel:
		req := newRequestFromMessage(&msg)
		cn.onPeerSentCancel(req)
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
		if err = cn.onPeerSentHaveAll(); err != nil {
			return msg, err
		}
		cn.updateRequests()
		return msg, nil
	case pp.HaveNone:
		// cn.cfg.debug().Println("peer claims it has nothing")
		if err = cn.peerSentHaveNone(); err != nil {
			return msg, err
		}
		cn.updateRequests()
		return msg, nil
	case pp.Reject:
		if !cn.supported(pp.ExtensionBitFast) {
			return msg, fmt.Errorf("reject recevied, fast not enabled")
		}

		req := newRequestFromMessage(&msg)
		cn.chunksRejected.Add(1)
		cn.clearRequests(req)
		return msg, nil
	case pp.AllowedFast:
		cn._mu.Lock()
		cn.fastset.AddRange(cn.t.chunks.Range(uint64(msg.Index)))
		cn._mu.Unlock()

		now := time.Now()
		if ts := cn.refreshrequestable.Swap(langx.Autoptr(now.Add(30 * time.Millisecond))); ts.After(now) {
			cn.updateRequests()
		}

		return msg, nil
	case pp.Extended:
		defer cn.updateRequests()
		if err = cn.onReadExtendedMsg(msg.ExtendedID, msg.ExtendedPayload); err != nil {
			return msg, err
		}
		return msg, nil
	default:
		return msg, errorsx.Errorf("received unknown message type: %#v", msg.Type)
	}
}

// Processes incoming BitTorrent wire-protocol messages. The client lock is held upon entry and
// exit. Returning will end the connection.
func (cn *connection) mainReadLoop(ctx context.Context) (err error) {
	cn.cfg.debug().Printf("c(%p) seed(%t) - read loop initiated\n", cn, cn.t.seeding())
	defer cn.cfg.debug().Printf("c(%p) seed(%t) - read loop completed\n", cn, cn.t.seeding())
	defer cn.updateRequests() // tap the writer so it'll clean itself up.

	decoder := pp.NewDecoder(cn.r, cn.t.chunks.pool)

	for {
		_, err := cn.ReadOne(ctx, decoder)
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

func (cn *connection) onReadExtendedMsg(id pp.ExtensionNumber, payload []byte) (err error) {
	t := cn.t

	switch id {
	case pp.HandshakeExtendedID:
		var d pp.ExtendedHandshakeMessage
		if err := bencode.Unmarshal(payload, &d); err != nil {
			cn.cfg.errors().Printf("c(%p) seed(%t) error parsing extended handshake message %d %q: %s\n", cn, cn.t.seeding(), id, payload, err)
			return errorsx.Wrap(err, "unmarshalling extended handshake payload")
		}

		if d.Reqq != 0 {
			cn.PeerMaxRequests = d.Reqq
		}
		cn.PeerClientName = d.V
		cn.PeerPrefersEncryption = d.Encryption
		cn.PeerExtensionIDs = d.M
		cn.cfg.debug().Printf("c(%p) seed(%t) extensions: %s\n", cn, cn.t.seeding(), spew.Sdump(d))

		if d.MetadataSize != 0 {
			if err = t.setMetadataSize(d.MetadataSize); err != nil {
				return errorsx.Wrapf(err, "setting metadata size to %d", d.MetadataSize)
			}
		}

		cn.requestPendingMetadata()

		cn.sendInitialPEX()

		// BUG no sending PEX updates yet
		return nil
	case pp.MetadataExtendedID:
		// log.Println("metadata extension available")
		return errorsx.Wrap(t.gotMetadataExtensionMsg(payload, cn), "handling metadata extension message")
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
		t.AddPeers(peers)
		return nil
	default:
		return errorsx.Errorf("unexpected extended message ID: %v", id)
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
func (cn *connection) receiveChunk(msg *pp.Message) error {
	req := newRequestFromMessage(msg)

	cn.clearRequests(req)

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
	cn.lastUsefulChunkReceived = time.Now()
	cn.chunksReceived.Add(1)

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

	cn.cmu().Lock()
	cn.touched.AddInt(cn.t.chunks.requestCID(req))
	cn.cmu().Unlock()

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

// clearRequests drops the requests from the local connection.
func (cn *connection) clearRequests(reqs ...request) (ok bool) {
	clearone := func(r request) bool {
		cn.cmu().Lock()
		defer cn.cmu().Unlock()
		if _, ok := cn.requests[r.Digest]; !ok {
			return false
		}

		delete(cn.requests, r.Digest)
		return true
	}

	for _, r := range reqs {
		ok = ok || clearone(r)
	}

	cn.updateExpectingChunks()
	cn.updateRequests()

	return ok
}

// releaseRequest returns the request back to the pool.
func (cn *connection) releaseRequest(r request) (ok bool) {
	cn.cmu().Lock()
	defer cn.cmu().Unlock()
	if r, ok = cn.requests[r.Digest]; !ok {
		return false
	}

	// cn.cfg.debug().Printf("c(%p) - releasing request d(%020d) r(%d,%d,%d)\n", cn, r.Digest, r.Index, r.Begin, r.Length)
	delete(cn.requests, r.Digest)
	cn.t.chunks.Retry(r)

	cn.updateExpectingChunks()
	cn.updateRequests()

	return true
}

func (cn *connection) dupRequests() (requests []request) {
	cn._mu.RLock()
	for _, r := range cn.requests {
		requests = append(requests, r)
	}
	cn._mu.RUnlock()
	return requests
}

func (cn *connection) deleteAllRequests() {
	reqs := cn.dupRequests()
	for _, r := range reqs {
		cn.releaseRequest(r)
	}
}

func (cn *connection) sendChunk(r request, msg func(pp.Message) bool) (more bool, err error) {
	// Count the chunk being sent, even if it isn't.
	b := make([]byte, r.Length)
	p := cn.t.info.Piece(int(r.Index))
	n, err := cn.t.readAt(b, p.Offset()+int64(r.Begin))
	if n != len(b) {
		if err == nil {
			panic("expected error")
		}
		return false, err
	} else if err == io.EOF {
		err = nil
	}

	more = msg(pp.NewPiece(r.Index, r.Begin, b))
	cn.lastChunkSent = time.Now()
	return more, nil
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
	f := pp.PexPeerFlags(0)
	if cn.PeerPrefersEncryption {
		f |= pp.PexPrefersEncryption
	}
	if cn.outgoing {
		f |= pp.PexOutgoingConn
	}
	return f
}

func (cn *connection) sendInitialPEX() {
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

	cn.Post(pp.NewExtended(cn.extension(pp.ExtensionNamePex), bencode.MustMarshal(m)))
}
