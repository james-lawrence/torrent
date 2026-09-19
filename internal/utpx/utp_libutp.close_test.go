//go:build cgo && !disable_libutp
// +build cgo,!disable_libutp

package utpx

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/stretchr/testify/require"
)

// TestSocketClose covers closing a socket right after closing a connection made through it.
func TestSocketClose(t *testing.T) {
	t.Run("gives closing packets time to leave", func(t *testing.T) {
		s, err := New("udp4", "127.0.0.1:0")
		require.NoError(t, err)

		start := time.Now()
		require.NoError(t, s.Close())

		require.GreaterOrEqual(t, time.Since(start), grace)
	})

	t.Run("peer sees the close when the socket is closed right after the connection", func(t *testing.T) {
		const payload = 256 * 1024

		for i := range 10 {
			listener, err := New("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() {
				errorsx.Log(listener.Close())
			}()

			dialer, err := New("udp4", "127.0.0.1:0")
			require.NoError(t, err)

			accepted := make(chan net.Conn, 1)
			go func() {
				c, _ := listener.Accept()
				accepted <- c
			}()

			ctx, done := context.WithTimeout(t.Context(), 5*time.Second)
			c, err := dialer.DialContext(ctx, "udp4", listener.Addr().String())
			done()
			require.NoError(t, err, "iteration %d", i)

			peer := <-accepted
			defer peer.Close()

			// enough data that the closing packet has to queue behind it.
			n, err := c.Write(make([]byte, payload))
			require.NoError(t, err, "iteration %d", i)
			require.Equal(t, payload, n, "iteration %d", i)

			require.NoError(t, c.Close())
			require.NoError(t, dialer.Close())

			require.NoError(t, peer.SetReadDeadline(time.Now().Add(5*time.Second)))
			received, err := io.Copy(io.Discard, peer)
			require.NoError(t, err, "iteration %d: peer never saw the connection close", i)
			require.EqualValues(t, payload, received, "iteration %d", i)
		}
	})
}
