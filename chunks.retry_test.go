package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetry(t *testing.T) {
	t.Run("an expired request never satisfied is returned to missing", func(t *testing.T) {
		p := quickpopulate(newChunks(256, tinyTorrentInfo()))

		reqs, err := p.Pop(1, p.missing.Clone())
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		r := reqs[0]
		cidx := p.requestCID(r)

		p.Retry(r)

		require.True(t, p.missing.ContainsInt(cidx))
		require.False(t, p.inflight.ContainsInt(cidx))
	})

	t.Run("a request already verified by another claimant is left alone", func(t *testing.T) {
		p := quickpopulate(newChunks(256, tinyTorrentInfo()))

		reqs, err := p.Pop(1, p.missing.Clone())
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		victim := reqs[0]
		cidx := p.requestCID(victim)

		// the chunk arrives and is verified - as if a different connection's
		// duplicate claim (endgame) won the race.
		require.NoError(t, p.Verify(victim))
		require.True(t, p.unverified.ContainsInt(cidx))

		// this connection's own now-redundant request times out afterward.
		p.Retry(victim)

		require.True(t, p.unverified.ContainsInt(cidx), "verified data must not be undone by a late duplicate timing out")
		require.False(t, p.missing.ContainsInt(cidx), "an already-verified chunk must not be resurrected into missing")
	})

	t.Run("a request whose piece already completed is left alone", func(t *testing.T) {
		p := quickpopulate(newChunks(256, tinyTorrentInfo()))

		reqs, err := p.Pop(1, p.missing.Clone())
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		victim := reqs[0]
		cidx := p.requestCID(victim)

		// the whole piece completes (e.g. every other chunk in it already
		// arrived through a different connection) before this request expires.
		require.True(t, p.Complete(uint64(victim.Index)))

		p.Retry(victim)

		require.False(t, p.missing.ContainsInt(cidx), "a chunk from an already-completed piece must not be resurrected into missing")
	})
}
