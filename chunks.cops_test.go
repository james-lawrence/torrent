package torrent

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/assert"
)

func TestCopRequestPool(t *testing.T) {
	t.Run("zero pieces returns missing only", func(t *testing.T) {
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 0, completed: roaring.New(), missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9))
	})

	t.Run("nothing completed returns missing only", func(t *testing.T) {
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: roaring.New(), missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9))
	})

	t.Run("more than 5 percent remaining returns missing only", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 90)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9), "a chunk already inflight to another connection must not be offered while far from completion")
	})

	t.Run("5 percent or less remaining also returns inflight", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 95)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.True(t, pool.ContainsInt(9), "near completion, a chunk already inflight to another connection must also be offered")
	})

	t.Run("fully completed also returns inflight", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 100)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.True(t, pool.ContainsInt(9))
	})
}
