package dht

import (
	"bytes"
	"net"
	"runtime/trace"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

// runtime/trace is process global, the capture is inspected for strings since event
// and task names live in the raw trace's string table.
func TestAnnounceTraversalTrace(t *testing.T) {
	var capture bytes.Buffer
	require.NoError(t, trace.Start(&capture))
	stopped := false
	defer func() {
		if !stopped {
			trace.Stop()
		}
	}()

	t.Run("records why an announce could not start", func(t *testing.T) {
		s, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, s, mustListen(":0"))
		defer s.Close()

		_, err = s.AnnounceTraversal(t.Context(), int160.Random(), AnnouncePeer(s, true))
		require.ErrorIs(t, err, ErrDHTNoInitialNodes)
	})

	t.Run("records the traversal and the announce to the closest nodes", func(t *testing.T) {
		rconn := mustListen("127.0.0.1:0")
		aconn := mustListen("127.0.0.1:0")

		receiver, err := NewServer(32)
		require.NoError(t, err)
		backgroundServe(t, receiver, rconn)
		defer receiver.Close()

		announcer, err := NewServer(
			32,
			OptionBootstrapFixedAddrs(NewAddr(rconn.LocalAddr().(*net.UDPAddr).AddrPort())),
		)
		require.NoError(t, err)
		backgroundServe(t, announcer, aconn)
		defer announcer.Close()

		a, err := announcer.AnnounceTraversal(t.Context(), int160.Random(), AnnouncePeer(announcer, true))
		require.NoError(t, err)
		defer a.Close()

		select {
		case <-a.Finished():
		case <-time.After(10 * time.Second):
			t.Fatal("the announce did not finish")
		}
	})

	trace.Stop()
	stopped = true

	require.NotZero(t, capture.Len())
	for _, expected := range []string{
		"dht.announce",
		"dht.infohash",
		"no starting nodes",
		"starting nodes=1",
		"dht.traversal",
		"traversal contacted=1",
		"dht.announce_peer",
		"announce_peer nodes=1 failed=0",
	} {
		// not require.Contains, a failure would print the binary capture.
		require.True(t, bytes.Contains(capture.Bytes(), []byte(expected)), "missing from trace: %s", expected)
	}

	// the announcer and receiver both listen on loopback, their addresses must not be traced.
	require.False(t, bytes.Contains(capture.Bytes(), []byte("127.0.0.1")), "a node address reached the trace")
}
