package torrent

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/missinggo/bitmap"
	"github.com/anacrolix/missinggo/prioritybitmap"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/pkg/errors"
)

const (
	defaultChunk = 16 * (1 << 10) // 16 KiB
)

// empty error signifies that the queue is empty.
type empty struct {
	Outstanding int
	Missing     int
}

func (t empty) Error() string {
	return fmt.Sprintf("empty queue: outstanding requests(%d) - missing requests(%d)", t.Outstanding, t.Missing)
}

type bmap interface {
	Contains(int) bool
}

type everybmap struct{}

func (t everybmap) Contains(int) bool { return true }

func chunksPerPiece(plength, clength int64) int64 {
	return int64(math.Ceil(float64(plength) / float64(clength)))
}

func numChunks(total, plength, clength int64) int64 {
	if total == 0 || plength == 0 {
		return 0
	}

	npieces := total / plength // golang floors integer division.
	remainder := total - (npieces * plength)
	chunksper := chunksPerPiece(plength, clength)
	rchunks := int64(math.Ceil(float64(remainder) / float64(clength)))
	// fmt.Println("numchunks", total, clength, plength, remainder, "chunking", npieces, chunksper, chunksper*npieces, rchunks, chunksper*npieces+rchunks)
	return (chunksper * npieces) + rchunks
}

func chunkOffset(pidx, cidx, plength, clength int64) int64 {
	cidx = cidx % chunksPerPiece(plength, clength)
	return cidx * clength
}

func chunkLength(total, cidx, plength, clength int64, maximum bool) int64 {
	chunksper := chunksPerPiece(plength, clength)

	maxlength := clength
	if clength > plength {
		maxlength = plength
	}

	if maximum {
		return (total % plength) - ((cidx % chunksper) * maxlength)
	}

	if cidx%chunksper == chunksper-1 && clength != plength {
		return plength % clength
	}

	return maxlength
}

func pindex(chunk, plength, clength int64) int64 {
	return chunk / chunksPerPiece(plength, clength)
}

func newChunks(clength int, m *metainfo.Info) *chunks {
	p := &chunks{
		mu:          &sync.RWMutex{},
		meta:        m,
		cmaximum:    numChunks(m.Length, m.PieceLength, int64(clength)),
		clength:     int64(clength),
		gracePeriod: time.Minute,
		outstanding: make(map[uint64]request),
	}

	// log.Printf("%p - LENGTH %d NUMCHUNKS %d - CHUNK LENGTH %d - PIECE LEGNTH %d\n", p, p.meta.Length, p.cmaximum, p.clength, p.meta.PieceLength)
	return p
}

// chunks manages retrieving specific chunks of the torrent. its concurrent safe,
// and automatically recovers chunks that were requested but not received.
// the goal here is to have a single source of truth for what chunks are outstanding.
type chunks struct {
	meta *metainfo.Info
	// mu   *sync.RWMutex
	mu rwmutex

	// chunk length
	clength int64
	// maximum valid chunk index.
	cmaximum int64

	// track the number of reaping requests.
	reapers int64

	// gracePeriod how long to wait before reaping outstanding requests.
	gracePeriod time.Duration

	// cache of pieces we need to get. calculated from various piece and
	// file priorities and completion states elsewhere.
	missing prioritybitmap.PriorityBitmap

	// The last time we requested a chunk. Deleting the request from any
	// connection will clear this value.
	outstanding map[uint64]request

	// cache of the pieces that need to be verified.
	unverified bitmap.Bitmap

	// cache of completed piece indices, this means they have been retrieved and verified.
	completed bitmap.Bitmap
}

// chunks returns the set of chunk id's for the given piece.
func (t *chunks) chunks(idx int) (cidxs []int) {
	cpp := chunksPerPiece(t.meta.PieceLength, t.clength)
	chunks := numChunks(t.meta.PieceLength, t.meta.PieceLength, t.clength)

	for i := int64(0); i < chunks; i++ {
		cidx := (idx * int(cpp)) + int(i)
		if int64(cidx) < t.cmaximum {
			cidxs = append(cidxs, cidx)
		}
	}

	return cidxs
}

func (t *chunks) request(cidx int64, prio int) (r request, err error) {
	if t.cmaximum <= cidx {
		return r, fmt.Errorf("chunk index out of range: %d - %d", cidx, t.cmaximum)
	}

	pidx := pindex(cidx, t.meta.PieceLength, t.clength)
	start := chunkOffset(pidx, cidx, t.meta.PieceLength, t.clength)
	length := chunkLength(t.meta.Length, cidx, t.meta.PieceLength, t.clength, cidx == t.cmaximum-1)
	return newRequest2(pp.Integer(pidx), pp.Integer(start), pp.Integer(length), prio), nil
}

func (t *chunks) requestCID(r request) int {
	return int((chunksPerPiece(t.meta.PieceLength, t.clength) * int64(r.Index)) + int64(r.Begin)/t.clength)
}

func (t *chunks) pindex(cidx int) int {
	return int(pindex(int64(cidx), t.meta.PieceLength, t.clength))
}

// ChunksMissing checks if the given piece has any missing chunks.
func (t *chunks) ChunksMissing(pid int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, c := range t.chunks(pid) {
		if t.missing.Contains(c) {
			return true
		}
	}

	return false
}

// ChunksHashing return true iff any chunk for the given piece has been marked as unverified.
func (t *chunks) ChunksHashing(pid int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, c := range t.chunks(pid) {
		if t.unverified.Contains(c) {
			return true
		}
	}

	return false
}

// ChunksComplete returns true iff all the chunks for the given piece has been marked as completed.
func (t *chunks) ChunksComplete(pid int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.completed.Contains(pid)
}

// Chunks returns the chunk requests for the given piece.
func (t *chunks) chunksRequests(idx int) (requests []request) {
	for _, cidx := range t.chunks(idx) {
		req, _ := t.request(int64(cidx), -1*idx)
		requests = append(requests, req)
	}

	return requests
}

func (t *chunks) ChunksAdjust(pid int, prio int) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.completed.Get(pid) {
		return false
	}

	for _, c := range t.chunks(pid) {
		oprio, _ := t.missing.GetPriority(c)
		tmp := oprio != prio && t.missing.Set(c, prio)
		// log.Output(2, fmt.Sprintf("%p CHUNK PRIORITY ADJUSTED: %d %s prios %d %d %t %d\n", t, c, fmt.Sprintf("(%d)", pid), oprio, prio, tmp, t.missing.Len()))
		changed = changed || tmp
	}
	return changed
}

// ChunksPend marks all the chunks for the given piece to the priority.
// returns true if any changes were made.
func (t *chunks) ChunksPend(idx int) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// log.Output(2, fmt.Sprintf("%p ChunksPend %v\n", t, t.chunks(idx)))
	for _, c := range t.chunksRequests(idx) {
		tmp := t.pend(c, c.Priority)
		changed = changed || tmp
	}

	return changed
}

// ChunksRelease releases all the chunks for the given piece back into the missing
// pool.
func (t *chunks) ChunksRelease(idx int) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// log.Output(2, fmt.Sprintf("%p ChunksPend %v\n", t, t.chunks(idx)))
	for _, c := range t.chunksRequests(idx) {
		tmp := t.release(c)
		changed = changed || tmp
	}
	return changed
}

// Chunks returns the chunk requests for the given piece.
func (t *chunks) Chunks(idx int) (requests []request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.chunksRequests(idx)
}

// Priority returns the priority of the first chunk based on the piece ID.
func (t *chunks) Priority(idx int) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	cidx := chunksPerPiece(t.meta.PieceLength, t.cmaximum) * int64(idx)

	if prio, ok := t.missing.GetPriority(int(cidx)); ok {
		return prio
	}

	return int(PiecePriorityNone)
}

func (t *chunks) peek(available bmap) (cidx int, req request, err error) {
	var (
		ok   bool
		prio int
	)

	t.mu.Lock()
	defer t.mu.Unlock()

	idx := -1

	// grab first missing chunk
	t.missing.IterTyped(func(i bitmap.BitIndex) bool {
		pid := pindex(int64(i), int64(t.meta.PieceLength), int64(t.clength))

		if available.Contains(int(pid)) {
			idx = i
			return false
		}

		return true
	})

	if idx < 0 {
		return cidx, req, empty{Outstanding: len(t.outstanding), Missing: t.missing.Len()}
	}

	if prio, ok = t.missing.GetPriority(idx); !ok {
		return cidx, req, fmt.Errorf("missing priority: %d", idx)
	}

	if req, err = t.request(int64(idx), prio); err != nil {
		return cidx, req, errors.Wrap(err, "invalid request")
	}

	return idx, req, nil
}

// Peek at the request based on availability.
func (t *chunks) Peek(available bmap) (req request, err error) {
	_, req, err = t.peek(available)
	return req, err
}

// Pop the next piece to request. this advances a chunk from missing to oustanding.
//
// TODO: it'd be nice to use available as the bitmap to pop from to avoid repeatedly
// having to scan from start to finish the missing bitmap on torrent that contain pieces
// that are near the end of the local priority list. which slows down all connections.
//
// instead we could Pop off the available bitmap and mark it in both bitmaps as outstanding.
// the outstanding request could track which available bitmap it belongs to.
// but this complicates some of the logic, and I'm leaving it to do once refactoring
// the locks and contention issues are resolved.
func (t *chunks) Pop(available bmap) (req request, err error) {
	var (
		cidx int
	)

	if cidx, req, err = t.peek(available); err != nil {
		return req, err
	}

	d := req.digest()
	// log.Printf("c(%p) Popping: d(%d - %d) r(%d,%d,%d)\n", t, d, cidx, req.Index, req.Begin, req.Length)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.Recover()

	t.outstanding[d] = req
	t.missing.Remove(cidx)

	return req, nil
}

// Recover initiate a collection of outstanding requests.
// this moves them back into the missing bitmap, allowing them to be requested again.
func (t *chunks) Recover() {
	if atomic.CompareAndSwapInt64(&t.reapers, 0, 1) {
		// log.Println("reaping initiated")
		t.reap(100 * time.Millisecond)
		// log.Println("reaping completed")
		atomic.CompareAndSwapInt64(&t.reapers, 1, 0)
	}
	// TODO: this exposes a bug. basically the mutex is unlocked before its locked.
	// might be a bug in the golang compiler.
	// if i := atomic.AddInt64(&t.reapers, 1); i == 5 {
	// 	go func() {
	// 		for {
	// 			t.mu.Lock()
	// 			t.reap(100 * time.Millisecond)
	// 			t.mu.Unlock()
	// 			if i := atomic.AddInt64(&t.reapers, -1); i < 0 {
	// 				return
	// 			}
	// 		}
	// 	}()
	// }
}

func (t *chunks) reap(window time.Duration) {
	ts := time.Now()
	recovered := 0
	scanned := 0
	if len(t.outstanding) == 0 {
		return
	}

	for _, req := range t.outstanding {
		scanned++

		if req.Reserved.Add(t.gracePeriod).Before(ts) {
			t.pend(req, req.Priority)
			delete(t.outstanding, req.digest())
			recovered++
		}

		// stop reaping after the window has elapsed.
		if time.Since(ts) > window {
			break
		}
	}

	// if recovered > 0 {
	// log.Println(recovered, "/", scanned, "recovered in", time.Since(ts))
	// }
}

func (t *chunks) release(r request) bool {
	d := r.digest()

	_, ok := t.outstanding[d]
	delete(t.outstanding, d)

	return ok
}

func (t *chunks) pend(r request, prio int) (changed bool) {
	cidx := t.requestCID(r)

	// unconditionally mark the chunk as missing.
	changed = t.missing.Set(cidx, prio)

	// remove from unverified.
	t.unverified.Remove(cidx)

	// remove from completed.
	t.completed.Remove(int(r.Index))

	// d := r.digest()
	// log.Output(3, fmt.Sprintf("c(%p) pending request: d(%20d - %d) r(%d,%d,%d) u(%t)", t, d, cidx, r.Index, r.Begin, r.Length, changed))
	return changed
}

// Missing returns the number of missing chunks
func (t *chunks) Missing() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Recover()
	return t.missing.Len()
}

// Outstanding returns a copy of the outstanding requests
func (t *chunks) Outstanding() (dup map[uint64]request) {
	t.mu.Lock()
	defer t.mu.Unlock()

	dup = make(map[uint64]request, 0)
	for i, r := range t.outstanding {
		dup[i] = r
	}
	return dup
}

// Pend forces a chunks to be added to the missing queue.
func (t *chunks) Pend(req request, prio int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Recover()

	return t.pend(req, prio)
}

// Release a request from the outstanding mapping.
func (t *chunks) Release(r request) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Recover()
	// d := r.digest()
	// cidx := t.requestCID(r)
	// log.Output(2, fmt.Sprintf("c(%p) released request: d(%20d - %d) r(%d,%d,%d)", t, d, cidx, r.Index, r.Begin, r.Length))
	return t.release(r)
}

// Verify mark the chunk for verification.
func (t *chunks) Verify(r request) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Recover()

	d := r.digest()
	cid := t.requestCID(r)

	delete(t.outstanding, d)
	t.missing.Remove(cid)
	t.unverified.Set(cid, true)

	// log.Printf("c(%p) marked for verification: d(%d - %d) i(%d) b(%d) l(%d)\n", t, d, cid, r.Index, r.Begin, r.Length)

	return nil
}

// Validate mark all the chunks of the given piece to be validated.
func (t *chunks) Validate(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, cid := range t.chunks(pid) {
		t.unverified.Set(cid, true)
	}
}

func (t *chunks) Complete(pid int) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// log.Output(2, fmt.Sprintf("c(%p) marked completed: i(%d)\n", t, pid))

	for _, cid := range t.chunks(pid) {
		tmp := t.missing.Remove(cid) || t.unverified.Remove(cid)
		changed = changed || tmp
	}

	t.completed.Set(pid, true)

	return changed
}