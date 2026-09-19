package torrent

import (
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// TestConnwriterRequestsGenrequests covers what the writer does once a connection has nothing left it may request.
func TestConnwriterRequestsGenrequests(t *testing.T) {
	t.Run("no work while other connections hold every chunk does not spin the writer", func(t *testing.T) {
		ws := newTestWriterState(t)

		// the initialization connwriterinit performs on a live writer, which newWriterState leaves out.
		ws.requestable = roaring.New()
		ws.lowrequestwatermark = max(1, int(ws.PeerMaxRequests.Load()/4))
		ws.chokeduntil = time.Now().Add(-time.Minute)
		ws.mutate(func(ws *writerstate) { ws.PeerChoked = false })

		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))

		ws.cmu().Lock()
		ws.claimed.AddRange(0, uint64(ws.t.chunks.cmaximum))
		ws.cmu().Unlock()
		ws.peerPiecesChanged()

		// another connection took every chunk, nothing is left for this one to pop.
		everything := roaring.New()
		everything.AddRange(0, uint64(ws.t.chunks.cmaximum))
		taken, err := ws.t.chunks.Pop(int(ws.t.chunks.cmaximum), everything)
		require.NoError(t, err)
		require.Len(t, taken, int(ws.t.chunks.cmaximum))

		var requested []request
		mw := messageWriter(func(m pp.Message) error {
			if m.Type == pp.Request {
				requested = append(requested, newRequestFromMessage(&m))
			}
			return nil
		})

		gen := _connwriterRequests{writerstate: ws}
		gen.genrequests(gen.determineInterest(mw), mw)
		require.Empty(t, requested, "every chunk is outstanding elsewhere, there is nothing to request")

		// a refresh due right now makes connwriteridle skip idling, the writer loops without waiting for anything.
		require.True(t, ws.refreshrequestable.Load().After(time.Now()), "the retry for chunks held elsewhere must be in the future, otherwise the writer busy loops")
	})
}
