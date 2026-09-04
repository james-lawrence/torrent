package torrent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/metainfo"
)

func newDigestsFromTorrent(t *torrent) digests {
	return newDigests(
		t.storage,
		t.piece,
		func(idx int, cause error) func() {
			// log.Printf("hashed %d - %v\n", idx, cause)
			// log.Printf("hashed %p %d / %d - %v", t.chunks, idx+1, t.chunks.pieces, cause)

			if t.chunks.Hashed(uint64(idx), cause) {
				if p := t.piece(idx); p != nil {
					n := p.Length()
					t.stats.BytesValidated.Add(n)
					t.cln.stats.BytesValidated.Add(n)
				}
			}

			t.pieceStateChanges.Publish(idx)

			return func() {
				if t.cln.torrents == nil {
					return
				}

				if err := t.cln.torrents.Sync(t.md.ID); err != nil {
					t.cln.config.errors().Printf("failed to record missing chunks bitmap: %s - %v\n", t.md.ID, err)
				}
			}
		},
	)
}

func newDigests(iora io.ReaderAt, retrieve func(int) *metainfo.Piece, complete func(int, error) func()) digests {
	if iora == nil {
		panic("digests require a storage implementation")
	}

	return digests{
		ReaderAt: iora,
		retrieve: retrieve,
		complete: complete,
		pending:  newBitQueue(),
		c:        sync.NewCond(&sync.Mutex{}),
	}
}

// digests is responsible correctness of received data.
type digests struct {
	ReaderAt io.ReaderAt
	retrieve func(int) *metainfo.Piece
	complete func(int, error) func()
	// marks whether digest is actively processing.
	reaping int64
	// cache of the pieces that need to be verified.
	pending   *bitQueue
	c         *sync.Cond
	completed atomic.Uint64
}

// Enqueue a piece to check its completed digest.
func (t *digests) Enqueue(idx uint64) {
	t.pending.Push(int(idx))
	t.verify()
}

func (t *digests) EnqueueBitmap(o *roaring.Bitmap) {
	t.pending.PushBitmap(o)
	t.verify()
}

// wait for the digests to be complete. pending.Count() alone is not a valid
// completion signal - bitQueue is backed by a roaring.Bitmap (a set), so
// Count() drops to 0 the instant an item is popped for processing, well
// before its check() (real file I/O + hashing) actually finishes. reaping
// tracks dispatched-but-not-yet-finished work and only reaches 0 once every
// popped item's check() has returned, so it - not queue occupancy - is the
// real "nothing outstanding" signal.
func (t *digests) Wait() {
	t.c.L.Lock()
	defer t.c.L.Unlock()

	for atomic.LoadInt64(&t.reaping) > 0 || t.pending.Count() > 0 {
		t.c.Wait()
	}
}

func (t *digests) verify() {
	if atomic.AddInt64(&t.reaping, 1) > int64(runtime.NumCPU()) {
		atomic.AddInt64(&t.reaping, -1)
		return
	}

	go func() {
		for idx, ok := t.pending.Pop(); ok; idx, ok = t.pending.Pop() {
			t.check(idx)
		}

		// Held across the terminal decrement so Wait's check-then-Wait
		// sequence (guarded by the same t.c.L) can't observe reaping>0,
		// then miss this Broadcast because it fired in the gap before
		// Wait() actually registered: synchronize the state
		// transition through the Cond's own lock.
		t.c.L.Lock()
		remaining := atomic.AddInt64(&t.reaping, -1)
		t.c.L.Unlock()

		if remaining == 0 {
			t.c.Broadcast()
		}
	}()
}

func (t *digests) check(idx int) {
	var (
		err    error
		digest metainfo.Hash
		p      *metainfo.Piece
	)

	if p = t.retrieve(idx); p == nil {
		t.complete(idx, fmt.Errorf("piece %d not found during digest", idx))
		return
	}

	if digest, err = t.compute(p); err != nil {
		t.complete(idx, err)
		return
	}

	if digest != p.Hash() {
		t.complete(idx, fmt.Errorf("piece %d digest mismatch %s != %s", idx, hex.EncodeToString(digest[:]), p.Hash().String()))
		return
	}

	trackmissing := t.complete(idx, nil)

	// persist missing chunks to disk
	if ts := t.completed.Add(1); ts%100 == 0 {
		trackmissing()
	}
}

func (t *digests) compute(p *metainfo.Piece) (ret metainfo.Hash, err error) {
	var (
		buf [32 * bytesx.KiB]byte
	)
	c := sha1.New()
	plen := p.Length()

	n, err := io.CopyBuffer(c, io.NewSectionReader(t.ReaderAt, p.Offset(), plen), buf[:])
	if err != nil {
		return ret, errorsx.Wrapf(err, "piece %d digest failed", p.Offset())
	}

	if n != plen {
		return ret, fmt.Errorf("piece digest failed short copy %d: %d != %d", p.Offset(), n, plen)
	}

	copy(ret[:], c.Sum(nil))

	return ret, nil
}
