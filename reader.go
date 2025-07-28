package torrent

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/storage"
)

func newBlockingReader(imp storage.TorrentImpl, c *chunks, d *digests) *blockingreader {
	return &blockingreader{
		TorrentImpl: imp,
		c:           c,
		d:           d,
	}
}

type blockingreader struct {
	storage.TorrentImpl
	d *digests
	c *chunks
}

func (t *blockingreader) ReadAt(p []byte, offset int64) (n int, err error) {
	var (
		allowed int64
		onceb   = atomicx.Bool(true)
	)

	pid := uint64(t.c.meta.OffsetToIndex(offset))

	t.c.cond.L.Lock()
	for allowed = t.c.DataAvailableForOffset(offset); allowed < 0; allowed = t.c.DataAvailableForOffset(offset) {
		if t.c.ChunksAvailable(pid) && onceb.CompareAndSwap(true, false) {
			t.d.Enqueue(pid)
		}

		t.c.cond.Wait()
	}
	t.c.cond.L.Unlock()

	allowed = min(allowed, int64(len(p)))
	return t.TorrentImpl.ReadAt(p[:allowed], offset)
}

// Reader for a torrent
type Reader interface {
	io.Reader
	io.Seeker
	io.Closer
}

func NewReader(t Torrent) Reader {
	return &reader{
		ReaderAt: t.Storage(),
		length:   t.Info().TotalLength(),
	}
}

// Accesses Torrent data via a Client. Reads block until the data is
// available. Seeks and readahead also drive Client behaviour.
type reader struct {
	io.ReaderAt
	// Adjust the read/seek window to handle Readers locked to File extents
	// and the like.
	offset, length int64
	pos            int64
}

var _ io.ReadCloser = &reader{}

func (r *reader) Read(b []byte) (n int, err error) {
	// log.Println("read initiated", r.pos, r.length)
	n, err = r.ReadAt(b, r.pos)
	atomic.AddInt64(&r.pos, int64(n)) // npos
	// log.Println("read completed", npos, r.length)
	return n, err
}

func (r *reader) Close() error {
	return nil
}

func (r *reader) Seek(off int64, whence int) (ret int64, err error) {
	switch whence {
	case io.SeekStart:
		atomic.SwapInt64(&r.pos, off)
		return off, nil
	case io.SeekCurrent:
		return atomic.AddInt64(&r.pos, off), nil
	case io.SeekEnd:
		atomic.SwapInt64(&r.pos, r.length-off)
		return r.pos, nil
	default:
		return -1, errors.ErrUnsupported
	}
}
