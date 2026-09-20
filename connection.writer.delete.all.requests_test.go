package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeleteAllRequestsLocked covers the writer's exit path, when a connection goes away every request it
// still has outstanding must be handed back to the pool otherwise the chunks stay inflight.
func TestDeleteAllRequestsLocked(t *testing.T) {
	t.Run("releases every outstanding request back to missing", func(t *testing.T) {
		ws := newTestWriterState(t)
		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		reqs, err := ws.t.chunks.Pop(int(ws.t.chunks.cmaximum), ws.t.chunks.missing.Clone())
		require.NoError(t, err)
		require.NotEmpty(t, reqs)

		ws.mutate(func(ws *writerstate) {
			for _, r := range reqs {
				ws.requests[r.Digest] = r
				ws.requested.AddInt(ws.t.chunks.requestCID(r))
			}
		})
		require.EqualValues(t, len(reqs), ws.t.chunks.Cardinality(ws.t.chunks.inflight))

		ws.mutate(func(ws *writerstate) { ws.deleteAllRequestsLocked() })

		require.Empty(t, ws.requests)
		require.True(t, ws.requested.IsEmpty())
		require.Zero(t, ws.t.chunks.Cardinality(ws.t.chunks.inflight), "closing a connection must not leave chunks inflight")
		require.EqualValues(t, len(reqs), ws.t.chunks.Cardinality(ws.t.chunks.missing))
	})
}
