package torrent

import (
	"context"
	"math"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// a peer is discarded after repeated failures, that is deliberate. what it must not do is stay reserved: the
// pool is how a download finds its seeds, and with a handful of trusted peers a discard that can't be undone
// stalls the torrent for good. the discarded peer has to be re-addable (see TunePeersPeriodic).
func TestPeerPoolDiscardedPeerCanBeReadded(t *testing.T) {
	newpool := func() peerPool {
		return newPeerPool(8, 3, func(Peer) peerPriority { return 0 })
	}
	addr := netip.MustParseAddrPort("10.0.0.1:2000")

	// the pool only recycles failed peers once a minute, let it do so whenever asked.
	pop := func(p *peerPool) (prioritizedPeer, bool) {
		p.nextswap = time.Time{}
		return p.PopMax()
	}

	// fail the peer until the pool gives up on it.
	exhaust := func(t *testing.T, pool *peerPool) {
		require.False(t, pool.Add(Peer{AddrPort: addr, Trusted: true}))

		for attempt := 1; attempt <= 10; attempt++ {
			popped, ok := pop(pool)
			if !ok {
				return
			}
			// what outgoingConnection does when the dial fails.
			pool.Attempted(popped.p, popped.p.Attempts+1)
		}
		t.Fatal("the pool never gave up on the peer")
	}

	t.Run("a discarded peer is not left loaned", func(t *testing.T) {
		pool := newpool()
		exhaust(t, &pool)

		_, loaned := pool.loaned[addr]
		require.False(t, loaned, "peer left loaned after being discarded")
	})

	t.Run("a discarded peer can be added again and handed out", func(t *testing.T) {
		pool := newpool()
		exhaust(t, &pool)

		require.False(t, pool.Add(Peer{AddrPort: addr, Trusted: true}), "re-adding a discarded peer was ignored")

		popped, ok := pop(&pool)
		require.True(t, ok)
		require.Equal(t, addr, popped.p.AddrPort)
		require.Zero(t, popped.p.Attempts, "a re-added peer starts over")
	})

	t.Run("re-adding a peer that is mid dial is a no-op", func(t *testing.T) {
		pool := newpool()
		require.False(t, pool.Add(Peer{AddrPort: addr, Trusted: true}))
		popped, ok := pop(&pool)
		require.True(t, ok)

		require.True(t, pool.Add(popped.p))
	})
}

// by default a peer is never discarded, however many times it fails.
func TestPeerPoolDefaultNeverDiscards(t *testing.T) {
	pool := newPeerPool(8, math.MaxUint64, func(Peer) peerPriority { return 0 })
	addr := netip.MustParseAddrPort("10.0.0.1:2000")
	require.False(t, pool.Add(Peer{AddrPort: addr, Trusted: true}))

	for attempt := 1; attempt <= 100; attempt++ {
		pool.nextswap = time.Time{}
		popped, ok := pool.PopMax()
		require.True(t, ok, "attempt %d: the peer was discarded", attempt)
		pool.Attempted(popped.p, popped.p.Attempts+1)
	}
}

// the loop ends when the torrent does.
func TestTunePeersPeriodicStopsWhenTheTorrentCloses(t *testing.T) {
	var calls atomic.Int64

	cl := newTestClient(t)
	md, err := NewFromMagnet("magnet:?xt=urn:btih:ZOCMZQIPFFW7OLLMIC5HUB6BPCSDEOQU")
	require.NoError(t, err)

	_, _, err = cl.Start(md, TunePeersPeriodic(t.Context(), 5*time.Millisecond, func() []Peer {
		calls.Add(1)
		return nil
	}))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return calls.Load() >= 5 }, time.Second, time.Millisecond)

	require.NoError(t, cl.Stop(md))
	// let a call that was already running finish.
	time.Sleep(50 * time.Millisecond)

	n := calls.Load()
	require.Never(t, func() bool { return calls.Load() > n }, 100*time.Millisecond, 5*time.Millisecond, "the loop kept running after the torrent closed")
}

// cancelling the context stops the loop.
func TestTunePeersPeriodicStopsWhenCancelled(t *testing.T) {
	var calls atomic.Int64

	cl := newTestClient(t)
	md, err := NewFromMagnet("magnet:?xt=urn:btih:ZOCMZQIPFFW7OLLMIC5HUB6BPCSDEOQU")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, _, err = cl.Start(md, TunePeersPeriodic(ctx, 5*time.Millisecond, func() []Peer {
		calls.Add(1)
		return nil
	}))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return calls.Load() >= 5 }, time.Second, time.Millisecond)

	cancel()
	// let a call that was already running finish.
	time.Sleep(50 * time.Millisecond)

	n := calls.Load()
	require.Never(t, func() bool { return calls.Load() > n }, 100*time.Millisecond, 5*time.Millisecond, "the loop kept running after the context was cancelled")
}
