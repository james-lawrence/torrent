package torrent

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/anacrolix/missinggo/pubsub"
	"github.com/anacrolix/missinggo/slices"
	"github.com/james-lawrence/torrent/bep0009"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/iox"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"github.com/james-lawrence/torrent/tracker"

	"github.com/james-lawrence/torrent/bencode"
	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
)

// Tuner runtime tuning of an actively running torrent.
type Tuner func(*torrent)

func TuneNoop(t *torrent) {}

// TuneMaxConnections adjust the maximum connections allowed for a torrent.
func TuneMaxConnections(m int) Tuner {
	return func(t *torrent) {
		t.SetMaxEstablishedConns(m)
	}
}

// TunePeers add peers to the torrent.
func TunePeers(peers ...Peer) Tuner {
	return func(t *torrent) {
		t.AddPeers(peers)
	}
}

// Reset tracking data for tracker events
func TuneResetTrackingStats(s *Stats) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()
		*s = t.Stats()
		t.stats = t.stats.ResetTransferMetrics()
	}
}

// Extract the peer id from the torrent
func TuneReadPeerID(id *int160.T) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()
		*id = t.cln.config.localID
	}
}

func TuneReadHashID(id *int160.T) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()
		*id = int160.FromByteArray(t.md.ID)
	}
}

func TuneReadUserAgent(v *string) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()
		*v = t.cln.config.HTTPUserAgent
	}
}

func TuneReadPublicIPv4(v net.IP) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()

		copy(v, t.cln.config.publicIP4)
	}
}

func TuneReadPublicIPv6(v net.IP) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()

		copy(v, t.cln.config.publicIP6)
	}
}

func TuneReadPort(v *int) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()

		*v = t.cln.LocalPort()
	}
}

func TuneReadBytesRemaining(v *int64) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()

		*v = t.bytesLeft()
		if *v < 0 {
			*v = 0
		}
	}
}

// The subscription emits as (int) the index of pieces as their state changes.
// A state change is when the PieceState for a piece alters in value.
func TuneSubscribe(sub *pubsub.Subscription) Tuner {
	return func(t *torrent) {
		*sub = *t.pieceStateChanges.Subscribe()
	}
}

func TuneReadAnnounce(v *tracker.Announce) Tuner {
	return func(t *torrent) {
		t.rLock()
		defer t.rUnlock()

		*v = t.cln.config.AnnounceRequest()
	}
}

// TuneClientPeer adds a trusted, pending peer for each of the Client's addresses.
// used for tests.
func TuneClientPeer(cl *Client) Tuner {
	return func(t *torrent) {
		ps := []Peer{}
		id := cl.PeerID().AsByteArray()
		for _, la := range cl.ListenAddrs() {
			ps = append(ps, Peer{
				ID:      id,
				IP:      langx.Must(netx.NetIP(la)),
				Port:    langx.Must(netx.NetPort(la)),
				Trusted: true,
			})
		}

		t.AddPeers(ps)
	}
}

func TuneDisableTrackers(t *torrent) {
	t.lock()
	defer t.unlock()
	t.md.Trackers = nil
}

// add trackers to the torrent.
func TuneTrackers(trackers ...string) Tuner {
	return func(t *torrent) {
		t.lock()
		defer t.unlock()
		t.md.Trackers = append(t.md.Trackers, trackers...)
	}
}

func TunePublicTrackers(trackers ...string) Tuner {
	return func(t *torrent) {
		go func() {
			<-t.GotInfo()

			if t.info.Private != nil && langx.Autoderef(t.info.Private) {
				return
			}

			t.lock()
			defer t.unlock()

			t.md.Trackers = append(t.md.Trackers, trackers...)
			t.maybeNewConns()
		}()
	}
}

// force new connections to be found
func TuneNewConns(t *torrent) {
	t.maybeNewConns()
}

// used after info has been received to mark all chunks missing.
// will only happen if missing and completed are zero.
func TuneAutoDownload(t *torrent) {
	if t.chunks.Cardinality(t.chunks.completed)+t.chunks.Cardinality(t.chunks.missing) > 0 {
		return
	}

	t.chunks.fill(t.chunks.missing, uint64(t.chunks.cmaximum))
}

// mark missing the provided range of bytes.
func TuneDownloadRange(offset int64, length int64) Tuner {
	return func(t *torrent) {
		min := t.chunks.PIDForOffset(uint64(offset))
		max := t.chunks.PIDForOffset(uint64(offset + length))
		min, _ = t.chunks.Range(min)
		_, max = t.chunks.Range(max)
		t.chunks.zero(t.chunks.missing)
		t.chunks.MergeInto(t.chunks.missing, bitmapx.Range(min, max))
	}
}

// Announce to trackers looking for at least one successful request that returns peers.
func TuneAnnounceOnce(options ...tracker.AnnounceOption) Tuner {
	return func(t *torrent) {
		go TrackerAnnounceUntil(context.Background(), t, func() bool {
			return true
		}, options...)
	}
}

func TuneAnnounceUntilComplete(t *torrent) {
	go TrackerAnnounceUntil(context.Background(), t, func() bool {
		return !t.chunks.Incomplete()
	})
}

// Verify the entirety of the torrent. will block
func TuneVerifyFull(t *torrent) {
	t.digests.EnqueueBitmap(bitmapx.Fill(t.chunks.pieces))
	t.digests.Wait()

	t.chunks.MergeInto(t.chunks.missing, t.chunks.failed)
	t.chunks.Locked(func() {
		t.chunks.unverified.AndNot(t.chunks.missing)
	})
	t.chunks.FailuresReset()
}

// Verify the provided byte range (expanding as needed to match piece boundaries). will block.
func TuneVerifyRange(offset, length int64) Tuner {
	return func(t *torrent) {
		min := t.chunks.PIDForOffset(uint64(offset))
		max := t.chunks.PIDForOffset(uint64(offset + length))

		t.digests.EnqueueBitmap(bitmapx.Range(min, max))
		t.digests.Wait()

		t.chunks.MergeInto(t.chunks.missing, t.chunks.failed)
		t.chunks.Locked(func() {
			t.chunks.unverified.AndNot(t.chunks.missing)
		})
		t.chunks.FailuresReset()
	}
}

// Verify the contents asynchronously
func TuneVerifyAsync(t *torrent) {
	t.digests.EnqueueBitmap(bitmapx.Fill(t.chunks.pieces))

	go func() {
		t.digests.Wait()

		t.chunks.MergeInto(t.chunks.missing, t.chunks.failed)
		t.chunks.Locked(func() {
			t.chunks.unverified.AndNot(t.chunks.missing)
		})
		t.chunks.FailuresReset()
	}()
}

// Verify a random selection of n pieces. will block until complete blocks.
// if any of the sampled pieces failed it'll perform a full verify. it always checks
// the first and last piece regardless of the random set, as a result at most n+2 pieces
// will be verified. this makes it easy to test certain behaviors live.
// NOTE: torrents with a low completed rate will almost always performa full verify.
// but since there will also be a smaller amount of data on disk this is a fair trade off.
func TuneVerifySample(n uint64) Tuner {
	return func(t *torrent) {
		t.digests.EnqueueBitmap(bitmapx.Random(t.chunks.pieces, min(n, t.chunks.pieces)))
		t.digests.Enqueue(0)
		t.digests.Enqueue(min(t.chunks.pieces-1, t.chunks.pieces)) // min to handle 0 case which causes a uint wrap around.
		t.digests.Wait()

		// if everything validated assume the torrent is good and mark it as fully complete.
		if t.chunks.failed.IsEmpty() {
			t.chunks.fill(t.chunks.completed, t.chunks.pieces)
			t.chunks.zero(t.chunks.unverified)
			t.chunks.zero(t.chunks.missing)
			return
		}

		TuneVerifyFull(t)
	}
}

// lets randomly verify some of the data.
// will block until complete.
func tuneVerifySample(unverified *roaring.Bitmap, n uint64) Tuner {
	return func(t *torrent) {
		t.chunks.InitFromUnverified(unverified)
		t.Tune(TuneVerifySample(8))
	}
}

func TuneSeeding(t *torrent) {
	t.chunks.MergeInto(t.chunks.completed, bitmapx.Fill(t.chunks.pieces))
}

func TuneRecordMetadata(t *torrent) {
	if t.Info() == nil {
		panic("cannot persist torrent metadata when missing info")
	}

	t.md.InfoBytes = t.metadataBytes
	errorsx.Log(errorsx.Wrap(t.cln.torrents.Write(t.Metadata()), "failed to perist torrent file"))
}

func tuneMerge(md Metadata) Tuner {
	return func(t *torrent) {
		t.md.DisplayName = langx.DefaultIfZero(t.md.DisplayName, md.DisplayName)
		t.md.Trackers = append(t.md.Trackers, md.Trackers...)

		if md.ChunkSize != t.md.ChunkSize && md.ChunkSize != 0 {
			log.Println("merging set chunk size")
			t.setChunkSize(md.ChunkSize)
		}
	}
}

// Torrent represents the state of a torrent within a client.
// interface is currently being used to ease the transition of to a cleaner API.
// Many methods should not be called before the info is available,
// see .Info and .GotInfo.
type Torrent interface {
	Metadata() Metadata
	Tune(...Tuner) error
	Stats() Stats
	BytesCompleted() int64        // TODO: maybe should be pulled from torrent, it has a reference to the storage implementation. or maybe part of the Stats call?
	Info() *metainfo.Info         // TODO: remove, this should be pulled from Metadata()
	GotInfo() <-chan struct{}     // TODO: remove, torrents should never be returned if they don't have the meta info.
	Storage() storage.TorrentImpl // temporary replacement for reader.
}

// Download a torrent into a writer blocking until completion.
func DownloadInto(ctx context.Context, dst io.Writer, m Torrent, options ...Tuner) (n int64, err error) {
	if err = m.Tune(options...); err != nil {
		return 0, err
	}

	select {
	case <-m.GotInfo():
	case <-ctx.Done():
		return 0, errorsx.Compact(context.Cause(ctx), ctx.Err())
	}

	if err = m.Tune(TuneRecordMetadata, TuneAutoDownload, TuneNewConns, TuneAnnounceOnce(tracker.AnnounceOptionEventStarted)); err != nil {
		return 0, err
	}

	if n, err = io.Copy(dst, NewReader(m)); err != nil {
		return n, err
	} else if n != m.Info().TotalLength() {
		return n, errorsx.Errorf("download failed, missing data %d != %d", n, m.Info().TotalLength())
	}

	if err = m.Tune(TuneAnnounceOnce(tracker.AnnounceOptionEventCompleted)); err != nil {
		log.Println("failed to announce completion", err)
	}

	return n, nil
}

// Return a ReadSeeker for the given range
func DownloadRange(ctx context.Context, m Torrent, off int64, length int64, options ...Tuner) (_ io.ReadSeeker) {
	if err := m.Tune(options...); err != nil {
		return iox.ErrReader(err)
	}

	select {
	case <-m.GotInfo():
	case <-ctx.Done():
		return iox.ErrReader(errorsx.Compact(context.Cause(ctx), ctx.Err()))
	}

	if err := m.Tune(TuneDownloadRange(off, length), TuneNewConns); err != nil {
		return iox.ErrReader(err)
	}

	return io.NewSectionReader(m.Storage(), off, length)
}

func Verify(ctx context.Context, t Torrent) error {
	if err := t.Tune(TuneNewConns); err != nil {
		return err
	}

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return errorsx.Compact(context.Cause(ctx), ctx.Err())
	}

	return t.Tune(TuneVerifyFull)
}

// returns a bitmap of the verified data within the storage implementation.
func VerifyStored(ctx context.Context, md *metainfo.MetaInfo, t io.ReaderAt) (missing *roaring.Bitmap, readable *roaring.Bitmap, _ error) {
	info, err := metainfo.NewInfoFromReader(bytes.NewReader(md.InfoBytes))
	if err != nil {
		return nil, nil, err
	}

	chunks := newChunks(defaultChunkSize, info)
	digests := newDigests(t, func(i int) *metainfo.Piece {
		return langx.Autoptr(info.Piece(i))
	}, func(idx int, cause error) func() {
		chunks.Hashed(uint64(idx), cause)
		return func() {}
	})

	digests.EnqueueBitmap(bitmapx.Fill(chunks.pieces))
	digests.Wait()

	chunks.MergeInto(chunks.missing, chunks.failed)
	chunks.FailuresReset()

	return chunks.Clone(chunks.missing), chunks.ReadableBitmap(), nil
}

func newTorrent(cl *Client, src Metadata, options ...Tuner) *torrent {
	const (
		maxEstablishedConns = 200
	)
	m := &sync.RWMutex{}
	chunkcond := sync.NewCond(&sync.Mutex{})

	t := &torrent{
		md:  src,
		cln: cl,
		_mu: m,
		peers: newPeerPool(32, func(p Peer) peerPriority {
			return bep40PriorityIgnoreError(cl.publicAddr(p.addr()), p.addr())
		}),
		conns:                   newconnset(2 * maxEstablishedConns),
		halfOpen:                make(map[string]Peer),
		_halfOpenmu:             &sync.RWMutex{},
		pieceStateChanges:       pubsub.NewPubSub(),
		storageOpener:           storage.NewClient(langx.DefaultIfZero(cl.config.defaultStorage, src.Storage)),
		maxEstablishedConns:     maxEstablishedConns,
		duplicateRequestTimeout: time.Second,
		chunks:                  newChunks(langx.DefaultIfZero(defaultChunkSize, src.ChunkSize), metainfo.NewInfo(), chunkoptCond(chunkcond)),
		pex:                     newPex(),
		storage:                 storage.NewZero(),
		digests:                 new(digests),
		wantPeersEvent:          make(chan struct{}, 1),
		closed:                  make(chan struct{}),
	}
	t.event = &sync.Cond{L: tlocker{torrent: t}}
	*t.digests = newDigestsFromTorrent(t)
	if err := t.setInfoBytes(src.InfoBytes); err != nil {
		log.Println("encountered an error setting info bytes", len(src.InfoBytes), err)
	}

	if err := t.Tune(options...); err != nil {
		log.Println("encountered an error tuning torrent", err)
	}

	return t
}

func newconnset(n int) *conns {
	return &conns{
		m: make(map[*connection]struct{}, n),
	}
}

type conns struct {
	_m sync.RWMutex
	m  map[*connection]struct{}
}

func (t *conns) insert(c *connection) {
	t._m.Lock()
	defer t._m.Unlock()
	t.m[c] = struct{}{}
}

func (t *conns) delete(c *connection) (int, bool) {
	t._m.Lock()
	defer t._m.Unlock()
	olen := len(t.m)
	delete(t.m, c)
	nlen := len(t.m)
	return nlen, olen != nlen
}

func (t *conns) filtered(ignore func(*connection) bool) (ret []*connection) {
	t._m.RLock()
	defer t._m.RUnlock()
	ret = make([]*connection, 0, len(t.m))
	for conn := range t.m {
		if ignore(conn) {
			continue
		}
		ret = append(ret, conn)
	}
	return ret
}

func (t *conns) list() (ret []*connection) {
	t._m.RLock()
	defer t._m.RUnlock()
	ret = make([]*connection, 0, len(t.m))
	for conn := range t.m {
		ret = append(ret, conn)
	}
	return ret
}

func (t *conns) length() int {
	t._m.RLock()
	defer t._m.RUnlock()
	return len(t.m)
}

// Maintains state of torrent within a Client.
type torrent struct {
	// Torrent-level aggregate statistics. First in struct to ensure 64-bit
	// alignment. See #262.
	stats ConnStats

	md               Metadata
	numReceivedConns int64

	cln *Client
	_mu *sync.RWMutex
	// Name used if the info name isn't available. Should be cleared when the
	// Info does become available.
	nameMu sync.RWMutex

	// How long to avoid duplicating a pending request.
	duplicateRequestTimeout time.Duration

	// closed         missinggo.Event
	closed chan struct{}

	// Values are the piece indices that changed.
	pieceStateChanges *pubsub.PubSub

	// The storage to open when the info dict becomes available.
	storageOpener *storage.Client
	// Storage for torrent data.
	storage storage.TorrentImpl
	// Read-locked for using storage, and write-locked for Closing.
	storageLock sync.RWMutex

	// The info dict. nil if we don't have it (yet).
	info  *metainfo.Info
	files []*File

	// Active peer connections, running message stream loops. TODO: Make this
	// open (not-closed) connections only.
	conns *conns

	maxEstablishedConns int

	// Set of addrs to which we're attempting to connect. Connections are
	// half-open until all handshakes are completed.
	halfOpen    map[string]Peer
	_halfOpenmu *sync.RWMutex

	// Reserve of peers to connect to. A peer can be both here and in the
	// active connections if were told about the peer after connecting with
	// them. That encourages us to reconnect to peers that are well known in
	// the swarm.
	peers          peerPool
	wantPeersEvent chan struct{}

	readabledataavailable atomic.Bool
	metainfoAvailable     atomic.Bool

	// The bencoded bytes of the info dict. This is actively manipulated if
	// the info bytes aren't initially available, and we try to fetch them
	// from peers.
	metadataBytes []byte
	// Each element corresponds to the 16KiB metadata pieces. If true, we have
	// received that piece.
	metadataCompletedChunks []bool

	// chunks management tracks the current status of the different chunks
	chunks *chunks

	// digest management determines if pieces are valid.
	digests *digests

	// peer exchange for the current torrent
	pex *pex

	// signal events on this torrent.
	event *sync.Cond
}

// Returns a Reader bound to the torrent's data. All read calls block until
// the data requested is actually available.
func (t *torrent) Storage() storage.TorrentImpl {
	return newBlockingReader(t.storage, t.chunks, t.digests)
}

// Metadata provides enough information to lookup the torrent again.
func (t *torrent) Metadata() Metadata {
	dup := t.md
	return dup
}

// Tune the settings of the torrent.
func (t *torrent) Tune(tuning ...Tuner) error {
	for _, opt := range tuning {
		opt(t)
	}

	return nil
}

func (t *torrent) _lock(depth int) {
	// updated := atomic.AddUint64(&t.lcount, 1)
	// log.Output(depth, fmt.Sprintf("t(%p) lock initiated - %d", t, updated))
	t._mu.Lock()
	// log.Output(depth, fmt.Sprintf("t(%p) lock completed - %d", t, updated))
}

func (t *torrent) _unlock(depth int) {
	// updated := atomic.AddUint64(&t.ucount, 1)
	// log.Output(depth, fmt.Sprintf("t(%p) unlock initiated - %d", t, updated))
	t._mu.Unlock()
	// log.Output(depth, fmt.Sprintf("t(%p) unlock completed - %d", t, updated))
}

func (t *torrent) _rlock(depth int) {
	// updated := atomic.AddUint64(&t.lcount, 1)
	// l2.Output(depth, fmt.Sprintf("t(%p) rlock initiated - %d", t, updated))
	t._mu.RLock()
	// l2.Output(depth, fmt.Sprintf("t(%p) rlock completed - %d", t, updated))
}

func (t *torrent) _runlock(depth int) {
	// updated := atomic.AddUint64(&t.ucount, 1)
	// l2.Output(depth, fmt.Sprintf("t(%p) unlock initiated - %d", t, updated))
	t._mu.RUnlock()
	// l2.Output(depth, fmt.Sprintf("t(%p) unlock completed - %d", t, updated))
}

func (t *torrent) lock() {
	t._lock(3)
}

func (t *torrent) unlock() {
	t._unlock(3)
}

func (t *torrent) rLock() {
	t._rlock(3)
}

func (t *torrent) rUnlock() {
	t._runlock(3)
}

// Returns a channel that is closed when the Torrent is closed.
func (t *torrent) Closed() <-chan struct{} {
	return t.closed
}

// KnownSwarm returns the known subset of the peers in the Torrent's swarm, including active,
// pending, and half-open peers.
func (t *torrent) KnownSwarm() (ks []Peer) {
	// Add pending peers to the list
	t.peers.Each(func(peer Peer) {
		ks = append(ks, peer)
	})

	t._halfOpenmu.RLock()
	// Add half-open peers to the list
	for _, peer := range t.halfOpen {
		ks = append(ks, peer)
	}
	t._halfOpenmu.RUnlock()

	// Add active peers to the list
	for _, conn := range t.conns.list() {
		ks = append(ks, Peer{
			ID:     conn.PeerID.AsByteArray(),
			IP:     conn.remoteAddr.Addr().AsSlice(),
			Port:   int(conn.remoteAddr.Port()),
			Source: conn.Discovery,
			// > If the connection is encrypted, that's certainly enough to set SupportsEncryption.
			// > But if we're not connected to them with an encrypted connection, I couldn't say
			// > what's appropriate. We can carry forward the SupportsEncryption value as we
			// > received it from trackers/DHT/PEX, or just use the encryption state for the
			// > connection. It's probably easiest to do the latter for now.
			// https://github.com/anacrolix/torrent/pull/188
			SupportsEncryption: conn.headerEncrypted,
		})
	}

	return ks
}

func (t *torrent) setChunkSize(size uint64) {
	t.md.ChunkSize = size
	// potential bug here use to be '*t.chunks = *newChunks(...)' change to straight assignment to deal with
	// Unlock called on a non-locked mutex.
	*t.chunks = *newChunks(size, langx.DefaultIfZero(metainfo.NewInfo(), t.info), chunkoptMutex(t.chunks.mu), chunkoptCond(t.chunks.cond), chunkoptCompleted(t.chunks.completed))
}

// There's a connection to that address already.
func (t *torrent) addrActive(addr string) bool {
	t._halfOpenmu.RLock()
	_, ok := t.halfOpen[addr]
	t._halfOpenmu.RUnlock()
	if ok {
		return true
	}

	for _, c := range t.conns.list() {
		ra := c.remoteAddr
		if ra.String() == addr {
			return true
		}
	}

	return false
}

func (t *torrent) unclosedConnsAsSlice() (ret []*connection) {
	return t.conns.filtered(func(c *connection) bool { return c.closed.Load() })
}

func (t *torrent) AddPeer(p Peer) {
	t.lock()
	defer t.unlock()
	t.addPeer(p)
}

func (t *torrent) addPeer(p Peer) {
	select {
	case <-t.closed:
		t.cln.config.debug().Printf("torrent.addPeer closed")
		return
	default:
	}

	if t.peers.Add(p) {
		metrics.Add("peers replaced", 1)
	}

	t.openNewConns()

	for t.peers.Len() > t.cln.config.TorrentPeersHighWater {
		if _, ok := t.peers.DeleteMin(); ok {
			metrics.Add("excess reserve peers discarded", 1)
		}
	}
}

func (t *torrent) invalidateMetadata() {
	for i := range t.metadataCompletedChunks {
		t.metadataCompletedChunks[i] = false
	}
	t.nameMu.Lock()
	t.info = nil
	t.nameMu.Unlock()
}

func (t *torrent) saveMetadataPiece(index int, data []byte) {
	if t.haveInfo() {
		return
	}

	if index >= len(t.metadataCompletedChunks) {
		t.cln.config.warn().Printf("%s: ignoring metadata piece %d\n", t, index)
		return
	}

	copy(t.metadataBytes[16*bytesx.KiB*index:], data)
	t.metadataCompletedChunks[index] = true
}

func (t *torrent) metadataPieceCount() int {
	return (len(t.metadataBytes) + (1 << 14) - 1) / (1 << 14)
}

func (t *torrent) haveMetadataPiece(piece int) bool {
	if t.haveInfo() {
		return (1<<14)*piece < len(t.metadataBytes)
	}

	return piece < len(t.metadataCompletedChunks) && t.metadataCompletedChunks[piece]
}

func (t *torrent) metadatalen() int {
	if t == nil {
		return 0
	}
	return len(t.metadataBytes)
}

func (t *torrent) setInfo(info *metainfo.Info) (err error) {
	if err := validateInfo(info); err != nil {
		return fmt.Errorf("bad info: %s", err)
	}

	if t.storageOpener != nil {
		t.storage, err = t.storageOpener.OpenTorrent(info, t.md.ID)
		if err != nil {
			return fmt.Errorf("error opening torrent storage: %T - %s", t.storageOpener, err)
		}
	}

	t.nameMu.Lock()
	t.info = info
	t.setChunkSize(langx.DefaultIfZero(defaultChunkSize, t.md.ChunkSize))
	t.md.InfoBytes = t.metadataBytes
	t.nameMu.Unlock()

	t.initFiles()

	return nil
}

func (t *torrent) onSetInfo() {
	for _, conn := range t.conns.list() {
		if err := conn.resetclaimed(); err != nil {
			t.cln.config.info().Println(errorsx.Wrap(err, "closing connection"))
			conn.Close()
		}
	}

	t.metainfoAvailable.Store(true)
	t.event.Broadcast()
	t.updateWantPeersEvent()
}

// Called when metadata for a torrent becomes available.
func (t *torrent) setInfoBytes(b []byte) error {
	var info metainfo.Info

	if len(b) == 0 {
		return nil
	}

	if id := metainfo.NewHashFromBytes(b); id != t.md.ID {
		return errorsx.Errorf("info bytes have wrong hash %d %s != %s", len(b), id.String(), t.md.ID.String())
	}

	if err := bencode.Unmarshal(b, &info); err != nil {
		return fmt.Errorf("error unmarshalling info bytes: %s", err)
	}

	if err := t.setInfo(&info); err != nil {
		return err
	}

	t.metadataBytes = b
	t.metadataCompletedChunks = nil
	*t.digests = newDigestsFromTorrent(t)

	t.onSetInfo()

	return nil
}

func (t *torrent) haveAllMetadataPieces() bool {
	if t.haveInfo() {
		return true
	}

	if t.metadataCompletedChunks == nil {
		return false
	}

	for _, have := range t.metadataCompletedChunks {
		if !have {
			return false
		}
	}

	return true
}

// TODO: Propagate errors to disconnect peer.
func (t *torrent) setMetadataSize(bytes int) (err error) {

	if t.haveInfo() {
		// We already know the correct metadata size.
		return err
	}

	if bytes <= 0 || bytes > 10*bytesx.MiB { // 10MB, pulled from my ass.
		return errorsx.New("bad size")
	}

	if t.metadataBytes != nil && len(t.metadataBytes) == int(bytes) {
		return err
	}

	t.metadataBytes = make([]byte, bytes)
	t.metadataCompletedChunks = make([]bool, (bytes+(1<<14)-1)/(1<<14))

	for _, c := range t.conns.list() {
		c.requestPendingMetadata()
	}

	return err
}

func (t *torrent) metadataPieceSize(piece int) int {
	return metadataPieceSize(len(t.metadataBytes), piece)
}

func (t *torrent) newMetadataExtensionMessage(c *connection, msgType int, piece int, data []byte) pp.Message {
	d := map[string]int{
		"msg_type": msgType,
		"piece":    piece,
	}

	if data != nil {
		d["total_size"] = len(t.metadataBytes)
	}

	p := append(bencode.MustMarshal(d), data...)
	return pp.NewExtended(c.PeerExtensionIDs[pp.ExtensionNameMetadata], p)
}

func (t *torrent) haveInfo() bool {
	return t.info != nil
}

func (t *torrent) bytesLeft() (left int64) {
	if !t.haveInfo() {
		return -1
	}

	s := t.chunks.Snapshot(&Stats{})

	return t.info.TotalLength() - ((int64(s.Unverified) * int64(t.chunks.clength)) + (int64(s.Completed) * int64(t.info.PieceLength)))
}

func (t *torrent) usualPieceSize() int {
	return int(t.info.PieceLength)
}

func (t *torrent) close() (err error) {
	t.lock()
	defer t.unlock()

	select {
	case <-t.closed:
	default:
		close(t.closed)
	}

	func() {
		if t.storage == nil {
			return
		}

		t.storageLock.Lock()
		defer t.storageLock.Unlock()
		t.storage.Close()
	}()

	for _, conn := range t.conns.list() {
		conn.Close()
	}

	t.event.Broadcast()

	return err
}

func (t *torrent) writeChunk(piece int, begin int64, data []byte) (err error) {
	if len(data) > int(t.info.PieceLength) {
		return fmt.Errorf("long write")
	}

	// calculate offset for chunk
	offset := (t.info.PieceLength * int64(piece)) + begin

	n, err := t.storage.WriteAt(data, offset)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}

	return err
}

func (t *torrent) pieceLength(piece uint64) pp.Integer {
	if t.info.PieceLength == 0 {
		// There will be no variance amongst pieces. Only pain.
		return 0
	}

	if piece == t.chunks.pieces-1 {
		ret := pp.Integer(t.info.TotalLength() % t.info.PieceLength)
		if ret == 0 {
			return pp.Integer(t.info.PieceLength)
		}

		return ret
	}
	return pp.Integer(t.info.PieceLength)
}

// The worst connection is one that hasn't been sent, or sent anything useful
// for the longest. A bad connection is one that usually sends us unwanted
// pieces, or has been in worser half of the established connections for more
// than a minute.
func (t *torrent) worstBadConn() *connection {
	wcs := worseConnSlice{t.unclosedConnsAsSlice()}
	heap.Init(&wcs)
	for wcs.Len() != 0 {
		c := heap.Pop(&wcs).(*connection)
		if c.stats.ChunksReadWasted.Int64() >= 6 && c.stats.ChunksReadWasted.Int64() > c.stats.ChunksReadUseful.Int64() {
			return c
		}
		// If the connection is in the worst half of the established
		// connection quota and is older than a minute.
		if wcs.Len() >= (t.maxEstablishedConns+1)/2 {
			// Give connections 1 minute to prove themselves.
			if time.Since(c.completedHandshake) > time.Minute {
				return c
			}
		}
	}
	return nil
}

func (t *torrent) incrementReceivedConns(c *connection, delta int64) {
	if c.Discovery == peerSourceIncoming {
		atomic.AddInt64(&t.numReceivedConns, delta)
	}
}

func (t *torrent) dropHalfOpen(addr string) {
	t._halfOpenmu.RLock()
	_, ok := t.halfOpen[addr]
	t._halfOpenmu.RUnlock()
	if !ok {
		t.cln.config.debug().Println("warning: attempted to drop a half open connection that doesn't exist")
		return
	}

	t._halfOpenmu.Lock()
	delete(t.halfOpen, addr)
	t._halfOpenmu.Unlock()
}

func (t *torrent) maybeNewConns() {
	// Tickle the accept routine.
	t.cln.event.Broadcast()
	t.openNewConns()
}

func (t *torrent) openNewConns() {
	var (
		ok bool
		p  Peer
	)

	for {
		if !t.wantConns() {
			t.cln.config.debug().Println("openNewConns: connections not wanted")
			return
		}

		if p, ok = t.peers.PopMax(); !ok {
			t.cln.config.debug().Println("openNewConns: no peers")
			return
		}

		t.cln.config.debug().Printf("initiating connection to peer %p %s %d\n", t, p.IP, p.Port)
		t.initiateConn(context.Background(), p)
	}
}

// Non-blocking read. Client lock is not required.
func (t *torrent) readAt(b []byte, off int64) (n int, err error) {
	return t.storage.ReadAt(b, off)
}

// Returns an error if the metadata was completed, but couldn't be set for
// some reason. Blame it on the last peer to contribute.
func (t *torrent) maybeCompleteMetadata(c *connection) error {
	if !t.haveAllMetadataPieces() {
		// Don't have enough metadata pieces.
		t.cln.config.debug().Printf("seeding(%t) %s: missing metadata from peers\n", t.seeding(), t)
		return nil
	}

	if err := t.setInfoBytes(t.metadataBytes); err != nil {
		t.invalidateMetadata()
		return fmt.Errorf("error setting info bytes: %s", err)
	}

	t.chunks.fill(t.chunks.missing, uint64(t.chunks.cmaximum))
	t.cln.config.debug().Printf("seeding(%t) %s: received metadata from peers\n", t.seeding(), t)

	return nil
}

func (t *torrent) needData() bool {
	select {
	case <-t.closed:
		return false
	default:
	}

	if !t.haveInfo() {
		return true
	}

	return t.chunks.Incomplete()
}

// Don't call this before the info is available.
func (t *torrent) bytesCompleted() int64 {
	if !t.haveInfo() {
		return 0
	}
	return t.info.TotalLength() - t.bytesLeft()
}

func (t *torrent) dropConnection(c *connection) {
	if t.deleteConnection(c) {
		t.openNewConns()
	}

	t.event.Broadcast()
}

// Returns true if connection is removed from torrent.Conns.
func (t *torrent) deleteConnection(c *connection) (ret bool) {
	t.pex.dropped(c)
	c.Close()
	// l2.Printf("closed c(%p) - pending(%d)\n", c, len(c.requests))
	nlen, ret := t.conns.delete(c)

	if nlen == 0 {
		t.assertNoPendingRequests()
	}

	return ret
}

func (t *torrent) assertNoPendingRequests() {
	if outstanding := t.chunks.Outstanding(); len(outstanding) != 0 {
		for _, r := range outstanding {
			t.cln.config.errors().Printf("still expecting c(%p) d(%020d) r(%d,%d,%d)", t.chunks, r.Digest, r.Index, r.Begin, r.Length)
		}
	}
}

func (t *torrent) wantPeers() bool {
	select {
	case <-t.closed:
		return false
	default:
	}

	if t.peers.Len() > t.cln.config.TorrentPeersLowWater {
		return false
	}
	return t.needData() || t.seeding()
}

func (t *torrent) updateWantPeersEvent() {
	if t.wantPeers() {
		select {
		case t.wantPeersEvent <- struct{}{}:
		default:
		}
	}
}

// Returns whether the client should make effort to seed the torrent.
func (t *torrent) seeding() bool {
	select {
	case <-t.closed:
		return false
	default:
	}

	if !t.cln.config.Seed {
		return false
	}

	if !t.haveInfo() {
		return false
	}

	// check if we have determined if readable data available.
	// if not, check and store the result.
	if t.readabledataavailable.Load() {
		return true
	}

	t.readabledataavailable.Store(t.chunks.Readable() > 0)

	return t.readabledataavailable.Load()
}

// Adds peers revealed in an announce until the announce ends, or we have
// enough peers.
func (t *torrent) consumeDhtAnnouncePeers(ctx context.Context, pvs <-chan dht.PeersValues) {
	for {
		select {
		case v, ok := <-pvs:
			if !ok {
				t.cln.config.debug().Println(int160.FromByteArray(t.md.ID), "peer events completed")
				return
			}

			peers := slicesx.MapTransform(func(cp dht.Peer) Peer {
				return Peer{
					IP:     cp.Addr().AsSlice(),
					Port:   int(cp.Port()),
					Source: peerSourceDhtGetPeers,
				}
			}, slicesx.Filter(func(v dht.Peer) bool { return v.Port() != 0 }, v.Peers...)...)

			t.cln.config.debug().Println("adding peers", len(peers))
			t.AddPeers(peers)
		case <-ctx.Done():
			return
		}
	}
}

func (t *torrent) announceToDht(impliedPort bool, s *dht.Server) error {
	ctx, done := context.WithTimeout(context.Background(), 5*time.Minute)
	defer done()

	ps, err := s.AnnounceTraversal(ctx, t.md.ID, dht.AnnouncePeer(impliedPort, t.cln.LocalPort()))
	if err != nil {
		return err
	}

	defer ps.Close()
	go t.consumeDhtAnnouncePeers(ctx, ps.Peers)

	select {
	case <-t.closed:
	case <-ctx.Done():
		return context.Cause(ctx)
	}

	return nil
}

func (t *torrent) dhtAnnouncer(s *dht.Server) {
	errdelay := time.Duration(0) // for the first run 0 delay to immediately find peers
	for {
		t.cln.config.debug().Println("dht ancouncer waiting for peers event", int160.FromByteArray(s.ID()), t.md.ID)
		select {
		case <-t.closed:
			return
		case <-time.After(errdelay):
		case <-t.wantPeersEvent:
			log.Println("dht ancouncing peers wanted event", int160.FromByteArray(s.ID()), t.md.ID)
		}

		t.stats.DHTAnnounce.Add(1)

		if err := t.announceToDht(true, s); err == nil {
			errdelay = time.Hour // when we succeeded wait an hour unless a wantPeersEvent comes in.
			t.cln.config.debug().Println("dht ancouncing completed", int160.FromByteArray(s.ID()))
			continue
		} else if errors.Is(err, dht.ErrDHTNoInitialNodes) {
			t.cln.config.errors().Println(t, err)
			errdelay = time.Minute
			continue
		} else {
			t.cln.config.errors().Println(t, errorsx.Wrap(err, "error announcing to DHT"))
			errdelay = time.Second
			continue
		}
	}
}

func (t *torrent) addPeers(peers []Peer) {
	for _, p := range peers {
		t.addPeer(p)
	}
}

func (t *torrent) Stats() Stats {
	t.rLock()
	defer t.rUnlock()
	return t.statsLocked()
}

func (t *torrent) statsLocked() (ret Stats) {
	ret.Seeding = t.seeding()
	ret.ActivePeers = len(t.conns.list())
	ret.HalfOpenPeers = len(t.halfOpen)
	ret.PendingPeers = t.peers.Len()
	t.chunks.Snapshot(&ret)

	// TODO: these can be moved to the connections directly.
	// moving it will reduce the need to iterate the connections
	// to compute the stats.
	ret.MaximumAllowedPeers = t.maxEstablishedConns

	ret.TotalPeers = t.numTotalPeers()

	ret.ConnStats = t.stats.Copy()
	return ret
}

// The total number of peers in the torrent.
func (t *torrent) numTotalPeers() int {
	peers := make(map[string]struct{})

	for _, c := range t.conns.list() {
		if c == nil {
			continue
		}

		ra := c.conn.RemoteAddr()
		if ra == nil {
			// It's been closed and doesn't support RemoteAddr.
			continue
		}
		peers[ra.String()] = struct{}{}
	}

	t._halfOpenmu.RLock()
	for addr := range t.halfOpen {
		peers[addr] = struct{}{}
	}
	t._halfOpenmu.RUnlock()

	t.peers.Each(func(peer Peer) {
		peers[fmt.Sprintf("%s:%d", peer.IP, peer.Port)] = struct{}{}
	})

	return len(peers)
}

// Reconcile bytes transferred before connection was associated with a
// torrent.
func (t *torrent) reconcileHandshakeStats(c *connection) {
	if c.stats != (ConnStats{
		// Handshakes should only increment these fields:
		BytesWritten: c.stats.BytesWritten,
		BytesRead:    c.stats.BytesRead,
	}) {
		panic("bad stats")
	}
	c.postHandshakeStats(func(cs *ConnStats) {
		cs.BytesRead.Add(c.stats.BytesRead.Int64())
		cs.BytesWritten.Add(c.stats.BytesWritten.Int64())
	})
	c.reconciledHandshakeStats = true
}

// Returns true if the connection is added.
func (t *torrent) addConnection(c *connection) (err error) {
	var (
		dropping []*connection
	)

	select {
	case <-t.closed:
		return errorsx.New("torrent closed")
	default:
	}

	for _, c0 := range t.conns.list() {
		if c.PeerID != c0.PeerID {
			continue
		}

		if !t.cln.config.dropDuplicatePeerIds {
			continue
		}

		if left, ok := c.hasPreferredNetworkOver(c0); ok && left {
			dropping = append(dropping, c0)
		} else {
			return errorsx.New("existing connection preferred")
		}
	}

	if tot := t.conns.length(); tot >= t.maxEstablishedConns {
		c := t.worstBadConn()
		if c == nil {
			return errorsx.Errorf("don't want conns %d >= %d", tot, t.maxEstablishedConns)
		}

		dropping = append(dropping, c)
	}

	t.conns.insert(c)
	t.pex.added(c)

	t.lock()
	defer t.unlock()

	for _, d := range dropping {
		t.dropConnection(d)
	}

	t.cln.config.debug().Printf("added connections c(%p)\n", c)
	return nil
}

func (t *torrent) wantConns() bool {
	select {
	case <-t.closed:
		return false
	default:
	}

	if !t.seeding() && !t.needData() {
		return false
	}

	if t.conns.length() >= t.maxEstablishedConns {
		return false
	}

	return true
}

func (t *torrent) SetMaxEstablishedConns(max int) (oldMax int) {
	oldMax = t.maxEstablishedConns
	t.maxEstablishedConns = max

	cset := t.conns.list()
	wcs := slices.HeapInterface(cset, worseConn)
	for drop := len(cset) - t.maxEstablishedConns; drop > -1 && wcs.Len() > 0; drop-- {
		t.cln.config.debug().Println("dropping connection", drop, wcs.Len())
		t.dropConnection(wcs.Pop().(*connection))
		t.cln.config.debug().Println("dropped connection", drop, wcs.Len())
	}

	t.openNewConns()
	return oldMax
}

// Start the process of connecting to the given peer for the given torrent if
// appropriate.
func (t *torrent) initiateConn(ctx context.Context, peer Peer) {
	if t.cln.config.localID.Cmp(int160.FromByteArray(peer.ID)) == 0 {
		t.cln.config.debug().Println("skipping connection to self based on peer id")
		return
	}

	addr := peer.addr()

	// ignore connections to self.
	if pubaddr := t.cln.publicAddr(addr); pubaddr.Compare(addr) == 0 {
		t.cln.config.debug().Println("skipping connection to self based on address", pubaddr, addr)
		return
	}

	if t.addrActive(addr.String()) {
		return
	}

	go func() {
		for {
			var (
				timedout errorsx.Timeout
			)

			t._halfOpenmu.Lock()
			t.halfOpen[addr.String()] = peer
			t._halfOpenmu.Unlock()

			// outgoing connection has a dial rate limit
			if err := t.cln.outgoingConnection(ctx, t, addr, peer.Source, peer.Trusted); err == nil {
				t.cln.config.debug().Printf("outgoing connection completed\n")
				return
			} else if errors.As(err, &timedout) {
				t.cln.config.debug().Printf("timeout detected, retrying %T - %v - %s - %t\n", err, err, timedout.Timedout(), peer.Trusted)
				time.Sleep(timedout.Timedout())
				continue
			} else if errorsx.Ignore(err, context.DeadlineExceeded, context.Canceled) != nil {
				t.cln.config.debug().Printf("outgoing connection failed %T - %v\n", errorsx.Compact(errorsx.Unwrap(err), err), err)
				return
			}
		}
	}()
}

func (t *torrent) noLongerHalfOpen(addr string) {
	t.dropHalfOpen(addr)
	t.openNewConns()
}

func (t *torrent) dialTimeout() time.Duration {
	return reducedDialTimeout(t.cln.config.MinDialTimeout, t.cln.config.NominalDialTimeout, t.cln.config.HalfOpenConnsPerTorrent, t.peers.Len())
}

func (t *torrent) piece(i int) *metainfo.Piece {
	if t.info == nil {
		return nil
	}

	tmp := t.info.Piece(i)
	return &tmp
}

// Returns a channel that is closed when the info (.Info()) for the torrent
// has become available.
func (t *torrent) GotInfo() <-chan struct{} {
	m := make(chan struct{})
	go func() {
		t.event.L.Lock()
		for !t.metainfoAvailable.Load() {
			t.event.Wait()
		}
		t.event.L.Unlock()
		close(m)
	}()
	return m
}

// Returns the metainfo info dictionary, or nil if it's not yet available.
func (t *torrent) Info() *metainfo.Info {
	t.rLock()
	defer t.rUnlock()
	return t.info
}

// Number of bytes of the entire torrent we have completed. This is the sum of
// completed pieces, and dirtied chunks of incomplete pieces. Do not use this
// for download rate, as it can go down when pieces are lost or fail checks.
// Sample Torrent.Stats.DataBytesRead for actual file data download rate.
func (t *torrent) BytesCompleted() int64 {
	t.rLock()
	defer t.rUnlock()
	return t.bytesCompleted()
}

// The completed length of all the torrent data, in all its files. This is
// derived from the torrent info, when it is available.
func (t *torrent) Length() int64 {
	return t.info.TotalLength()
}

func (t *torrent) initFiles() {
	var offset int64
	for _, fi := range t.info.UpvertedFiles() {
		var path []string
		if len(fi.PathUTF8) != 0 {
			path = fi.PathUTF8
		} else {
			path = fi.Path
		}
		t.files = append(t.files, &File{
			t,
			strings.Join(append([]string{t.info.Name}, path...), "/"),
			offset,
			fi.Length,
			fi,
			// PiecePriorityNone,
		})
		offset += fi.Length
	}
}

// Returns handles to the files in the torrent. This requires that the Info is
// available first.
func (t *torrent) Files() []*File {
	return t.files
}

func (t *torrent) AddPeers(pp []Peer) {
	t.lock()
	defer t.unlock()
	t.addPeers(pp)
}

func (t *torrent) String() string {
	if s := t.md.DisplayName; s != "" {
		return strconv.Quote(s)
	}

	return t.md.ID.String()
}

func (t *torrent) ping(addr net.UDPAddr) {
	t.cln.eachDhtServer(func(s *dht.Server) {
		go func() {
			ret := dht.Ping3S(context.Background(), s, dht.NewAddr(&addr), s.ID())
			if errorsx.Ignore(ret.Err, context.DeadlineExceeded) != nil {
				t.cln.config.debug().Println("failed to ping address", ret.Err)
			}
		}()
	})
}

// Process incoming ut_metadata message.
func (t *torrent) gotMetadataExtensionMsg(payload []byte, c *connection) error {
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
			c.Post(t.newMetadataExtensionMessage(c, pp.RejectMetadataExtensionMsgType, d.Index, nil))
			return nil
		}
		start := 16 * bytesx.KiB * d.Index
		end := start + t.metadataPieceSize(d.Index)
		c.Post(t.newMetadataExtensionMessage(c, pp.DataMetadataExtensionMsgType, d.Index, t.metadataBytes[start:end]))
		return nil
	case pp.DataMetadataExtensionMsgType:
		// log.Printf("c(%p) seed(%t) RECEIVED METADATA %s\n", c, t.seeding(), spew.Sdump(d))
		c.allStats(add(1, func(cs *ConnStats) *count { return &cs.MetadataChunksRead }))
		if !c.requestedMetadataPiece(d.Index) {
			return fmt.Errorf("got unexpected piece %d", d.Index)
		}
		c.metadataRequests[d.Index] = false
		begin := len(payload) - metadataPieceSize(d.Total, d.Index)
		if begin < 0 || begin >= len(payload) {
			return fmt.Errorf("data has bad offset in payload: %d", begin)
		}

		// log.Printf("c(%p) seed(%t) METADATA SAVE INITIATED %s\n", c, t.seeding(), spew.Sdump(d))
		// defer log.Printf("c(%p) seed(%t) METADATA SAVED %s\n", c, t.seeding(), spew.Sdump(d))

		t.saveMetadataPiece(d.Index, payload[begin:])
		c.lastUsefulChunkReceived = time.Now()
		return t.maybeCompleteMetadata(c)
	case pp.RejectMetadataExtensionMsgType:
		return nil
	default:
		return errorsx.New("unknown msg_type value")
	}
}

type tlocker struct {
	*torrent
}

func (t tlocker) Lock() {
	t._lock(4)
}

func (t tlocker) Unlock() {
	t._unlock(4)
}
