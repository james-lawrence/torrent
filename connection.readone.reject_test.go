package torrent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// TestReadOneReject covers what happens to the chunks we requested from a peer when it answers with a
// reject. a rejected request is one the peer will never serve, if the chunk is not handed back to the pool
// it stays inflight forever: outside of endgame only missing chunks are requested, so the pieces holding
// leaked chunks can never complete and the download stalls.
func TestReadOneReject(t *testing.T) {
	t.Run("rejected request is returned to missing and no longer inflight", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection
		cn.PeerExtensionBytes.SetBit(pp.ExtensionBitFast)

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
		require.True(t, ws.t.chunks.inflight.ContainsInt(cidx), "popped chunk must be inflight before the reject")

		d := pp.NewDecoder(bytes.NewReader(pp.Message{Type: pp.Reject, Index: r.Index, Begin: r.Begin, Length: r.Length}.MustMarshalBinary()), cn.t.chunks.pool)
		msg, err := cn.ReadOne(t.Context(), d, ws)
		require.NoError(t, err)
		require.Equal(t, pp.Reject, msg.Type)

		require.Zero(t, ws.requestsLen(), "the connection no longer waits on a rejected request")
		require.True(t, ws.requested.IsEmpty(), "the connection no longer waits on a rejected request")
		require.False(t, ws.t.chunks.inflight.ContainsInt(cidx), "a rejected chunk must not stay inflight, nobody is going to deliver it")
		require.True(t, ws.t.chunks.missing.ContainsInt(cidx), "a rejected chunk must be requestable again")
	})

	t.Run("every rejected request in a batch is released", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection
		cn.PeerExtensionBytes.SetBit(pp.ExtensionBitFast)

		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		reqs, err := ws.t.chunks.Pop(int(ws.t.chunks.cmaximum), ws.t.chunks.missing.Clone())
		require.NoError(t, err)
		require.Greater(t, len(reqs), 1, "need more than one request to exercise a batch")

		ws.mutate(func(ws *writerstate) {
			for _, r := range reqs {
				ws.requests[r.Digest] = r
				ws.requested.AddInt(ws.t.chunks.requestCID(r))
			}
		})

		var buf bytes.Buffer
		for _, r := range reqs {
			buf.Write(pp.Message{Type: pp.Reject, Index: r.Index, Begin: r.Begin, Length: r.Length}.MustMarshalBinary())
		}

		d := pp.NewDecoder(&buf, cn.t.chunks.pool)
		for range reqs {
			_, err := cn.ReadOne(t.Context(), d, ws)
			require.NoError(t, err)
		}

		require.Zero(t, ws.requestsLen())
		require.Zero(t, ws.t.chunks.Cardinality(ws.t.chunks.inflight), "no chunk may stay inflight once the peer rejected everything we asked for")
		require.EqualValues(t, len(reqs), ws.t.chunks.Cardinality(ws.t.chunks.missing), "every rejected chunk must be requestable again")
	})

	t.Run("reject without the fast extension is an error and leaves the request outstanding", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection

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

		d := pp.NewDecoder(bytes.NewReader(pp.Message{Type: pp.Reject, Index: r.Index, Begin: r.Begin, Length: r.Length}.MustMarshalBinary()), cn.t.chunks.pool)
		_, err = cn.ReadOne(t.Context(), d, ws)
		require.Error(t, err)

		require.Equal(t, 1, ws.requestsLen(), "the connection is dropped on this error, the request is released with the connection")
		require.True(t, ws.t.chunks.inflight.ContainsInt(cidx))
	})
}
