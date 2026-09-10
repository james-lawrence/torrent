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
		c := &chunks{chunkstate: chunkstate{pieces: 0, endgame: 0.15, completed: roaring.New(), missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9))
	})

	t.Run("nothing completed returns missing only", func(t *testing.T) {
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, endgame: 0.15, completed: roaring.New(), missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9))
	})

	t.Run("more than 15 percent remaining returns missing only", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 80)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, endgame: 0.15, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.False(t, pool.ContainsInt(9), "a chunk already inflight to another connection must not be offered while far from completion")
	})

	t.Run("15 percent or less remaining also returns inflight", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 85)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, endgame: 0.15, completed: completed, missing: missing, inflight: inflight}}

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
		c := &chunks{chunkstate: chunkstate{pieces: 100, endgame: 0.15, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.True(t, pool.ContainsInt(9))
	})

	t.Run("endgame fraction is configurable", func(t *testing.T) {
		// 20% remaining: past the 0.15 default, but within a wider 0.25 override.
		completed := roaring.New()
		completed.AddRange(0, 80)
		missing := roaring.New()
		missing.AddInt(5)
		inflight := roaring.New()
		inflight.AddInt(9)
		c := &chunks{chunkstate: chunkstate{pieces: 100, endgame: 0.25, completed: completed, missing: missing, inflight: inflight}}

		pool := copRequestPool(c)
		assert.True(t, pool.ContainsInt(5))
		assert.True(t, pool.ContainsInt(9), "a wider configured endgame fraction must trigger earlier than the 15% default")
	})
}
