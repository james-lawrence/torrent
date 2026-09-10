package torrent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReapExpiredRequestsLocked(t *testing.T) {
	t.Run("leaves requests within the grace period untouched", func(t *testing.T) {
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

		ws.t.chunks.gracePeriod = time.Hour

		var reaped int
		ws.mutate(func(ws *writerstate) { reaped = ws.reapExpiredRequestsLocked() })

		require.Zero(t, reaped)
		require.Len(t, ws.requests, len(reqs))
		require.EqualValues(t, len(reqs), ws.requested.GetCardinality())
		require.Zero(t, ws.t.chunks.Cardinality(ws.t.chunks.missing))
	})

	t.Run("releases requests past the grace period back to missing", func(t *testing.T) {
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

		ws.t.chunks.gracePeriod = -time.Second

		var reaped int
		ws.mutate(func(ws *writerstate) { reaped = ws.reapExpiredRequestsLocked() })

		require.Equal(t, len(reqs), reaped)
		require.Empty(t, ws.requests)
		require.True(t, ws.requested.IsEmpty())
		require.EqualValues(t, len(reqs), ws.t.chunks.Cardinality(ws.t.chunks.missing))
	})

	t.Run("only releases the expired subset, leaving fresh requests in flight", func(t *testing.T) {
		ws := newTestWriterState(t)
		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		reqs, err := ws.t.chunks.Pop(int(ws.t.chunks.cmaximum), ws.t.chunks.missing.Clone())
		require.NoError(t, err)
		require.Greater(t, len(reqs), 1, "need at least 2 requests to exercise a mixed sweep")

		ws.mutate(func(ws *writerstate) {
			for _, r := range reqs {
				ws.requests[r.Digest] = r
				ws.requested.AddInt(ws.t.chunks.requestCID(r))
			}
		})

		ws.t.chunks.gracePeriod = time.Hour

		expired := reqs[0]
		ws.mutate(func(ws *writerstate) {
			r := ws.requests[expired.Digest]
			r.Reserved = time.Now().Add(-2 * time.Hour)
			ws.requests[expired.Digest] = r
		})

		var reaped int
		ws.mutate(func(ws *writerstate) { reaped = ws.reapExpiredRequestsLocked() })

		require.Equal(t, 1, reaped)
		require.Len(t, ws.requests, len(reqs)-1)
		require.NotContains(t, ws.requests, expired.Digest)
		require.EqualValues(t, 1, ws.t.chunks.Cardinality(ws.t.chunks.missing))
	})
}
