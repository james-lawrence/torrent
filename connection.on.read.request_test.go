package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// TestConnectionOnReadRequest covers what a seeder does with a request that arrives while it still has the
// peer choked. connections start choked, and a peer is handed its allowed fast set in the opening
// messages, so it can (and does) ask for those pieces before the writer has unchoked it.
func TestConnectionOnReadRequest(t *testing.T) {
	t.Run("choked peer requesting an allowed fast piece is queued", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection

		require.NoError(t, cn.t.Tune(TuneSeeding))
		cn.PeerExtensionBytes.SetBit(pp.ExtensionBitFast)
		cn.Choked.Store(true)
		cn.cmu().Lock()
		cn.peerfastset.Add(0)
		cn.cmu().Unlock()

		require.NoError(t, cn.onReadRequest(newRequest(0, 0, 1024), ws))

		// BEP 6, a peer may request its allowed fast pieces while choked. dropping the request leaves the
		// peer waiting on an answer that never comes, it was neither served nor rejected.
		require.Equal(t, 1, cn.peerRequestsLen(), "allowed fast request from a choked peer must be queued to be served")
		require.Zero(t, wsBufferLen(ws), "allowed fast request must not be rejected")
	})

	t.Run("choked peer requesting a piece outside the allowed fast set is rejected", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection

		require.NoError(t, cn.t.Tune(TuneSeeding))
		cn.PeerExtensionBytes.SetBit(pp.ExtensionBitFast)
		cn.Choked.Store(true)
		cn.cmu().Lock()
		cn.peerfastset.Add(0)
		cn.cmu().Unlock()

		require.NoError(t, cn.onReadRequest(newRequest(1, 0, 1024), ws))

		require.Zero(t, cn.peerRequestsLen(), "request outside the allowed fast set must not be queued while choked")
		require.NotZero(t, wsBufferLen(ws), "the peer must be told the request was rejected")
	})

	t.Run("choked peer without the fast extension is dropped", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection

		require.NoError(t, cn.t.Tune(TuneSeeding))
		cn.Choked.Store(true)

		require.NoError(t, cn.onReadRequest(newRequest(0, 0, 1024), ws))

		require.Zero(t, cn.peerRequestsLen(), "a choked peer without the fast extension can't be answered")
		require.Zero(t, wsBufferLen(ws), "there is no reject message without the fast extension")
	})

	t.Run("unchoked peer request is queued", func(t *testing.T) {
		ws := newTestWriterState(t)
		cn := ws.connection

		require.NoError(t, cn.t.Tune(TuneSeeding))
		cn.PeerExtensionBytes.SetBit(pp.ExtensionBitFast)
		cn.Choked.Store(false)

		require.NoError(t, cn.onReadRequest(newRequest(1, 0, 1024), ws))

		require.Equal(t, 1, cn.peerRequestsLen())
		require.Zero(t, wsBufferLen(ws))
	})
}
