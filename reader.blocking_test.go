package torrent

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/stretchr/testify/require"
)

// blockingReaderFixture wires up a blockingreader against real, correctly
// hashed torrent data on disk, without any client/network machinery.
type blockingReaderFixture struct {
	r           *blockingreader
	c           *chunks
	pieceLength int64
	data        []byte
}

func newBlockingReaderFixture(t *testing.T, npieces int) blockingReaderFixture {
	const pieceLength = int64(bytesx.KiB)

	dir := t.TempDir()
	info, _, err := torrenttest.Random(dir, uint64(npieces)*uint64(pieceLength), metainfo.OptionPieceLength(pieceLength))
	require.NoError(t, err)
	require.EqualValues(t, npieces, info.NumPieces())

	encoded, err := metainfo.Encode(info)
	require.NoError(t, err)
	id := metainfo.NewHashFromBytes(encoded)

	data, err := os.ReadFile(filepath.Join(dir, id.String()))
	require.NoError(t, err)

	impl, err := storage.NewFile(dir).OpenTorrent(info, int160.FromByteArray(id))
	require.NoError(t, err)
	t.Cleanup(func() { impl.Close() })

	c := newChunks(uint64(pieceLength), info)
	d := newDigests(impl, func(idx int) *metainfo.Piece {
		return langx.Autoptr(info.Piece(idx))
	}, func(idx int, cause error) func() {
		c.Hashed(uint64(idx), cause)
		return func() {}
	})

	return blockingReaderFixture{
		r:           newBlockingReader(impl, c, &d),
		c:           c,
		pieceLength: pieceLength,
		data:        data,
	}
}

// blocked asserts fn has not returned within a short window, proving the
// call is still blocked. it returns a channel that receives fn's result
// whenever it eventually does return.
func blocked(t *testing.T, fn func() (int, error)) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := fn()
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("expected call to block, but it returned: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	return done
}

func TestBlockingReader(t *testing.T) {
	t.Run("ReadAt blocks until the piece is explicitly completed", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 3)
		defer f.r.Close()

		buf := make([]byte, f.pieceLength)
		done := blocked(t, func() (int, error) { return f.r.ReadAt(buf, f.pieceLength*2) })

		f.c.Complete(2)
		require.True(t, f.c.ChunksComplete(2))

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("expected read to unblock after piece completed")
		}

		require.Equal(t, f.data[f.pieceLength*2:f.pieceLength*3], buf)
	})

	t.Run("ReadAt returns immediately once the piece is already complete", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 1)
		defer f.r.Close()

		f.c.Complete(0)
		require.True(t, f.c.ChunksComplete(0))

		buf := make([]byte, f.pieceLength)
		started := time.Now()
		n, err := f.r.ReadAt(buf, 0)
		require.NoError(t, err)
		require.Less(t, time.Since(started), 100*time.Millisecond)
		require.EqualValues(t, f.pieceLength, n)
		require.Equal(t, f.data, buf)
	})

	t.Run("ReadAt caps the read to the requested buffer size", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 1)
		defer f.r.Close()

		f.c.Complete(0)
		require.True(t, f.c.ChunksComplete(0))

		buf := make([]byte, f.pieceLength/2)
		n, err := f.r.ReadAt(buf, 0)
		require.NoError(t, err)
		require.EqualValues(t, f.pieceLength/2, n)
		require.Equal(t, f.data[:f.pieceLength/2], buf)
	})

	t.Run("ReadAt triggers a digest check when the piece is unverified, unblocking on success", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 2)
		defer f.r.Close()

		// mark the piece's chunks as received but not yet hashed. the
		// blockingreader should notice this and enqueue a digest check on
		// its own, rather than blocking forever. the check runs
		// asynchronously and can complete faster than any window we could
		// poll for "still blocked", so unlike the other cases here we only
		// assert that it does eventually unblock with the right data.
		f.c.Validate(1)
		require.True(t, f.c.ChunksAvailable(1))

		buf := make([]byte, f.pieceLength)
		done := make(chan error, 1)
		go func() {
			_, err := f.r.ReadAt(buf, f.pieceLength)
			done <- err
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("expected read to unblock once the digest check completed")
		}

		require.Equal(t, f.data[f.pieceLength:f.pieceLength*2], buf)
		require.True(t, f.c.ChunksComplete(1))
	})

	t.Run("Close unblocks pending reads with io.ErrClosedPipe", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 1)

		buf := make([]byte, f.pieceLength)
		done := blocked(t, func() (int, error) { return f.r.ReadAt(buf, 0) })

		require.NoError(t, f.r.Close())

		select {
		case err := <-done:
			require.ErrorIs(t, err, io.ErrClosedPipe)
		case <-time.After(5 * time.Second):
			t.Fatal("expected blocked read to unblock after Close")
		}
	})

	t.Run("ReadAt returns data instead of closed-pipe when completion and Close race", func(t *testing.T) {
		f := newBlockingReaderFixture(t, 1)

		buf := make([]byte, f.pieceLength)
		done := blocked(t, func() (int, error) { return f.r.ReadAt(buf, 0) })

		f.c.Complete(0)
		require.NoError(t, f.r.Close())

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("expected read to unblock with the completed piece's data")
		}

		require.Equal(t, f.data, buf)
	})
}
