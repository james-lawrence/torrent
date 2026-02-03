package dht

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUPnPPortForward(t *testing.T) {
	fallback := netip.MustParseAddrPort("192.168.1.100:6881")

	t.Run("yields fallback when no devices found", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		seq, err := UPnPPortForward(ctx, "test-id", 6881, fallback)
		require.NoError(t, err)

		for addr := range seq {
			require.Equal(t, fallback, addr)
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("context cancellation stops iterator", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		seq, err := UPnPPortForward(ctx, "test-id", 6881, fallback)
		require.NoError(t, err)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range seq {
				cancel()
			}
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("iterator did not stop after context cancellation")
		}
	})
}
