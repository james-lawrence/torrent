package torrent

import (
	"bytes"
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/cstate"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/metainfo"
)

// TestConnReaderIdleRespectsUploadBackoff proves connreaderidle does not let
// a pending, unrelated peer request (needsresponse) bypass an
// already-computed, not-yet-elapsed upload rate-limit backoff
// (uploadavailable). An eager peer that keeps sending requests must not be
// able to starve the seed's own upload rate limiter by repeatedly forcing
// connreaderidle straight back to connreaderactive -> upload() before the
// backoff deadline is reached.
func TestConnReaderIdleRespectsUploadBackoff(t *testing.T) {
	cfg := TestingConfig(t, t.TempDir(), ClientConfigSeed(true))
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()

	ts, err := New(metainfo.Hash{})
	require.NoError(t, err)
	tt := newTorrent(cl, ts)
	require.NoError(t, tt.setInfo(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*3),
		Length:      24 * (1 << 10),
		PieceLength: 8 * (1 << 10),
	}))

	c := cl.newConnection(nil, false, netip.AddrPort{})
	c.setTorrent(tt)
	// a peer request just arrived, exactly what updateRequests() sets on
	// every incoming pp.Request message.
	c.needsresponse.Store(true)

	ctx, done := context.WithCancel(t.Context())
	defer done()

	ws := &readerstate{
		connection:       c,
		keepAliveTimeout: 10 * time.Minute,
		chokeduntil:      time.Now().Add(-time.Minute),
		// a rate-limited upload() call just computed a backoff that hasn't
		// elapsed yet.
		uploadavailable: atomicx.Pointer(time.Now().Add(2 * time.Second)),
		seed:            true,
		Idler:           cstate.Idle(ctx, c.upload),
		requestbuffer:   new(bytes.Buffer),
		pool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, defaultChunkSize)
				return &b
			},
		},
	}
	defer ws.Idler.Stop()

	next := connreaderidle(ws)

	_, bypassed := next.(_connreaderAllowRequests)
	require.False(t, bypassed, "connreaderidle must not bypass a pending upload backoff just because needsresponse is set")
}

// TestConnReaderAllowRequestsDoesNotMutateChokeState proves the reader loop
// no longer decides whether to choke/unchoke the peer. That decision is made
// concurrently by the writer loop's _connwriterRequests.determineInterest
// using the exact same condition (seed || chokeduntil.After(now)) - having
// both goroutines write cn.Choked with no synchronization is a data race
// (confirmed via -race: cn.Choked was the highest-frequency race reported
// across repeated runs of TestSocketsBindSockets/TestClientTransferVarious).
// The reader must only read Choked (e.g. in upload()), never write it.
func TestConnReaderAllowRequestsDoesNotMutateChokeState(t *testing.T) {
	cfg := TestingConfig(t, t.TempDir(), ClientConfigSeed(true))
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()

	ts, err := New(metainfo.Hash{})
	require.NoError(t, err)
	tt := newTorrent(cl, ts)
	require.NoError(t, tt.setInfo(&metainfo.Info{
		Pieces:      make([]byte, metainfo.HashSize*3),
		Length:      24 * (1 << 10),
		PieceLength: 8 * (1 << 10),
	}))

	c := cl.newConnection(nil, false, netip.AddrPort{})
	c.setTorrent(tt)
	require.True(t, c.Choked, "sanity: connections start choked")

	ctx, done := context.WithCancel(t.Context())
	defer done()

	ws := &readerstate{
		connection:       c,
		keepAliveTimeout: 10 * time.Minute,
		// seed=true is exactly the condition under which the old reader-side
		// logic would have called Unchoke(), flipping c.Choked to false.
		chokeduntil:     time.Now().Add(-time.Minute),
		uploadavailable: atomicx.Pointer(time.Now()),
		seed:            true,
		Idler:           cstate.Idle(ctx, c.upload),
		requestbuffer:   new(bytes.Buffer),
		pool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, defaultChunkSize)
				return &b
			},
		},
	}
	defer ws.Idler.Stop()

	_connreaderAllowRequests{readerstate: ws, next: connReaderUpload(ws)}.Update(ctx, nil)

	require.True(t, c.Choked, "reader loop must not decide choke state - that must be left solely to the writer loop")
}
