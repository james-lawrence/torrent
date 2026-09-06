package torrent

import (
	"bytes"
	"crypto/sha1"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/metainfo"
	"github.com/stretchr/testify/require"
)

// blockingReaderAt blocks the first ReadAt call until release is closed, so a
// test can deterministically observe digests.Wait() while a check() is known
// to still be in flight - no timing luck required, unlike the underlying bug
// (which needs real scheduling variance to manifest without this).
type blockingReaderAt struct {
	data    []byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	n := copy(p, r.data[off:])
	return n, nil
}

// gatedReaderAt parks every ReadAt until release is closed, and counts how many
// are parked, so a test can hold a known number of digest workers inside check()
// and know when they are all there.
type gatedReaderAt struct {
	data    []byte
	parked  atomic.Int64
	release chan struct{}
}

func (r *gatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.parked.Add(1)
	<-r.release
	return copy(p, r.data[off:]), nil
}

// TestDigestsWaitBlocksUntilInFlightCheckCompletes proves Wait() does not
// return while a dispatched check() is still running. bitQueue is a set
// (backed by a roaring.Bitmap), so pending.Count() drops to 0 the instant an
// item is popped for processing - well before its check() (real file I/O +
// hashing) actually finishes. Wait() looping on pending.Count() alone can
// therefore return successfully while verification is still in flight,
// which is exactly what let TuneVerifySample read FailedEmpty() before the
// in-flight check had a chance to record a failure, incorrectly marking an
// undownloaded torrent fully complete.
func TestDigestsWaitBlocksUntilInFlightCheckCompletes(t *testing.T) {
	data := []byte("0123456789abcdef")
	hash := sha1.Sum(data)
	info := &metainfo.Info{Length: int64(len(data)), PieceLength: int64(len(data)), Pieces: hash[:]}

	r := &blockingReaderAt{data: data, started: make(chan struct{}), release: make(chan struct{})}

	var (
		mu        sync.Mutex
		completed []int
	)
	d := newDigests(r, func(idx int) *metainfo.Piece {
		p := info.Piece(idx)
		return &p
	}, func(idx int, cause error) func() {
		mu.Lock()
		completed = append(completed, idx)
		mu.Unlock()
		return func() {}
	})

	d.Enqueue(0)
	<-r.started // check(0) has popped the item and is now blocked in ReadAt.

	waitReturned := make(chan struct{})
	go func() {
		d.Wait()
		close(waitReturned)
	}()

	select {
	case <-waitReturned:
		t.Fatal("Wait returned before the in-flight check completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(r.release)

	select {
	case <-waitReturned:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after the in-flight check completed")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int{0}, completed, "check must have completed and reported before Wait returns")
}

// TestDigestsEnqueueNeverStrandsWork proves every enqueued piece is eventually
// checked, even when verify() declines to dispatch a worker.
//
// verify() refuses to dispatch once reaping has reached NumCPU, on the
// assumption that a live worker will pop what was just pushed. That assumption
// breaks when every worker is between its last empty Pop() and its decrement:
// they still hold their slots, so the push is refused, and then they all retire
// without ever seeing it. The piece stays in pending forever - Wait() never
// returns, and a live download's BytesCompleted() stops short of the total.
//
// No timing luck is required to hit it. The retire path takes t.c.L, so a test
// holding that lock pins every worker in the window on purpose: saturate the
// slots with workers parked in ReadAt, release them so they all pop an empty
// queue, and they stack up on the lock we hold - past their last Pop, still
// counted. Pushing then reproduces the bug exactly.
func TestDigestsEnqueueNeverStrandsWork(t *testing.T) {
	workers := runtime.NumCPU()

	block := []byte("0123456789abcdef")
	hash := sha1.Sum(block)

	// one more piece than there are worker slots: the last one is the piece
	// pushed into the retire window.
	pieces := workers + 1
	data := bytes.Repeat(block, pieces)
	digest := bytes.Repeat(hash[:], pieces)
	info := &metainfo.Info{Length: int64(len(data)), PieceLength: int64(len(block)), Pieces: digest}

	r := &gatedReaderAt{data: data, release: make(chan struct{})}

	var checked sync.Map
	d := newDigests(r, func(idx int) *metainfo.Piece {
		p := info.Piece(idx)
		return &p
	}, func(idx int, cause error) func() {
		require.NoError(t, cause)
		checked.Store(idx, struct{}{})
		return func() {}
	})

	// fill every slot. each enqueued piece parks its worker in ReadAt, so the
	// next Enqueue finds the queue occupied and dispatches another worker,
	// until reaping reaches NumCPU and verify() starts refusing.
	for idx := range workers {
		d.Enqueue(uint64(idx))
	}

	require.Eventually(t, func() bool {
		return r.parked.Load() == int64(workers)
	}, 10*time.Second, time.Millisecond, "every worker slot should be occupied")

	// hold the lock the workers retire through, then let them go: they finish
	// their checks, pop an empty queue, and block here rather than retiring.
	d.c.L.Lock()
	close(r.release)

	require.Eventually(t, func() bool {
		return d.pending.Count() == 0
	}, 10*time.Second, time.Millisecond, "workers should have drained the queue")

	// draining the queue is not the same as reaching the lock, and the two are
	// only a few instructions apart. there is nothing to observe in between -
	// but there is also nothing else a worker can do here except block on the
	// lock we hold, so settling for a moment is enough. erring low only costs
	// the reproduction, never a false failure.
	time.Sleep(100 * time.Millisecond)

	// ...and now the piece they must not miss. This is Enqueue, split: the push
	// runs inline so it is known to land inside the window, and verify() is
	// dispatched separately because a correct one blocks on the lock we hold.
	d.pending.Push(workers)

	verified := make(chan struct{})
	go func() {
		d.verify()
		close(verified)
	}()

	// a verify() that declines to dispatch returns without touching the lock,
	// so waiting for it pins the bug precisely. a correct one is still blocked
	// on the lock at this point and the timeout is the expected path.
	select {
	case <-verified:
	case <-time.After(100 * time.Millisecond):
	}

	d.c.L.Unlock()

	waited := make(chan struct{})
	go func() {
		d.Wait()
		close(waited)
	}()

	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		require.Failf(t, "Wait never returned", "%d pieces stranded in the queue with no worker left to drain them", d.pending.Count())
	}

	_, ok := checked.Load(workers)
	require.True(t, ok, "the piece pushed while the workers were retiring was never checked")
}
