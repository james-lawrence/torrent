package torrent

import (
	"crypto/sha1"
	"sync"
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
