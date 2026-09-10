package torrent

import (
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// TestGenrequests proves genrequests' dynamic lowrequestwatermark adjustment
// never drops below 1, matching its own doc comment ("with a floor of a
// single request") which the code didn't actually enforce - only an upper
// clamp against PeerMaxRequests existed. A burst of rejects in one cycle
// (chunksRejected far exceeding chunksReceived*4) drove the watermark
// negative, which zeroed genrequests' Pop budget (max(0, watermark-inflight))
// with no error to signal it - the connection would go quiet and only claw
// back up by at most +1 per cycle.
func TestGenrequests(t *testing.T) {
	t.Run("a burst of rejects does not zero out the request budget", func(t *testing.T) {
		ws := newTestWriterState(t)
		ws.requestable = roaring.New()
		ws.lowrequestwatermark = 4
		ws.chokeduntil = time.Now().Add(-time.Minute)
		ws.mutate(func(ws *writerstate) { ws.PeerChoked = false })

		ws.t.chunks.fill(ws.t.chunks.missing, uint64(ws.t.chunks.cmaximum))
		ws.cmu().Lock()
		ws.claimed.AddRange(0, uint64(ws.t.chunks.cmaximum))
		ws.cmu().Unlock()
		ws.peerPiecesChanged()

		// simulate a burst of BEP6 rejects (connection.go:819) far outweighing
		// anything actually received this cycle.
		ws.chunksReceived.Store(0)
		ws.chunksRejected.Store(100)

		var requested []request
		mw := messageWriter(func(m pp.Message) error {
			if m.Type == pp.Request {
				requested = append(requested, newRequestFromMessage(&m))
			}
			return nil
		})

		gen := _connwriterRequests{writerstate: ws}
		gen.genrequests(gen.determineInterest(mw), mw)

		require.EqualValues(t, 1, ws.lowrequestwatermark, "watermark must floor at 1, never go to 0 or negative")
		require.NotEmpty(t, requested, "a floored-but-positive watermark must still let the connection request work")
	})
}
