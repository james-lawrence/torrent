package torrent

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/sockets"
)

// slowsocket is a socket that takes a while to close.
type slowsocket struct {
	sockets.Socket
	delay  time.Duration
	closed *atomic.Int64
}

func (t slowsocket) Close() error {
	time.Sleep(t.delay)
	t.closed.Add(1)
	return nil
}

func TestClientCloseSockets(t *testing.T) {
	t.Run("closes every socket before returning", func(t *testing.T) {
		var closed atomic.Int64

		cl := &Client{conns: []sockets.Socket{
			slowsocket{delay: time.Millisecond, closed: &closed},
			slowsocket{delay: time.Millisecond, closed: &closed},
			slowsocket{delay: time.Millisecond, closed: &closed},
		}}

		cl.closeSockets()

		require.EqualValues(t, 3, closed.Load())
	})

	t.Run("closes the sockets concurrently", func(t *testing.T) {
		const delay = 200 * time.Millisecond

		var closed atomic.Int64

		cl := &Client{conns: []sockets.Socket{
			slowsocket{delay: delay, closed: &closed},
			slowsocket{delay: delay, closed: &closed},
			slowsocket{delay: delay, closed: &closed},
			slowsocket{delay: delay, closed: &closed},
		}}

		start := time.Now()
		cl.closeSockets()

		require.EqualValues(t, 4, closed.Load())
		// sequentially this would take 4 * delay.
		require.Less(t, time.Since(start), 2*delay)
	})

	t.Run("does nothing without sockets", func(t *testing.T) {
		require.NotPanics(t, (&Client{}).closeSockets)
	})
}
