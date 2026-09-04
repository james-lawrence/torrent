package asynccompute

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPoolRunRejectsAlreadyCanceledContext(t *testing.T) {
	pool := New(func(ctx context.Context, w int) error { return nil }, Backlog[int](64))
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 500; i++ {
		require.Error(t, pool.Run(ctx, i), "Run must not silently accept work once ctx is already canceled, even when the queue has room to enqueue it")
	}
}
