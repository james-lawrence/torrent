package torrent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// failingWriteStorage is a storage.TorrentImpl whose writes always fail.
type failingWriteStorage struct{}

func (failingWriteStorage) ReadAt([]byte, int64) (int, error) { return 0, errors.New("read failed") }
func (failingWriteStorage) WriteAt([]byte, int64) (int, error) {
	return 0, errors.New("write failed")
}
func (failingWriteStorage) Close() error { return nil }

// TestReceiveChunk covers what happens to a requested chunk when its data arrives. once the connection
// stops tracking the request the chunk must end up somewhere the torrent will notice: unverified when the
// data was stored, or back in missing when it could not be. a chunk left inflight is never requested again
// (outside of endgame only missing chunks are requested) and stalls the pieces it belongs to.
func TestReceiveChunk(t *testing.T) {
	t.Run("a chunk that fails to write is returned to missing and no longer inflight", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection
		cn.t.storage = failingWriteStorage{}

		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		reqs, err := ws.t.chunks.Pop(1, ws.t.chunks.missing.Clone())
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		r := reqs[0]
		cidx := ws.t.chunks.requestCID(r)

		ws.mutate(func(ws *writerstate) {
			ws.requests[r.Digest] = r
			ws.requested.AddInt(cidx)
		})

		err = cn.receiveChunk(&pp.Message{Type: pp.Piece, Index: r.Index, Begin: r.Begin, Piece: make([]byte, r.Length)}, ws)
		require.Error(t, err, "the failed write must be reported")

		require.False(t, ws.t.chunks.inflight.ContainsInt(cidx), "a chunk that could not be stored must not stay inflight, nobody is going to deliver it")
		require.True(t, ws.t.chunks.missing.ContainsInt(cidx), "a chunk that could not be stored must be requestable again")
		require.False(t, ws.t.chunks.unverified.ContainsInt(cidx), "nothing was stored, the chunk cannot be unverified")
	})

	t.Run("a chunk that is already available is released", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection
		cn.t.storage = failingWriteStorage{}

		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		reqs, err := ws.t.chunks.Pop(1, ws.t.chunks.missing.Clone())
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		r := reqs[0]
		cidx := ws.t.chunks.requestCID(r)

		ws.mutate(func(ws *writerstate) {
			ws.requests[r.Digest] = r
			ws.requested.AddInt(cidx)
		})

		// another connection already delivered the chunk (endgame duplicate).
		ws.t.chunks.unverified.AddInt(cidx)

		require.NoError(t, cn.receiveChunk(&pp.Message{Type: pp.Piece, Index: r.Index, Begin: r.Begin, Piece: make([]byte, r.Length)}, ws))

		require.Zero(t, ws.requestsLen())
		require.False(t, ws.t.chunks.inflight.ContainsInt(cidx), "the duplicate must not leave the chunk inflight")
		require.True(t, ws.t.chunks.unverified.ContainsInt(cidx), "the chunk delivered by the other connection is untouched")
	})
}
