package torrent

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/assert"
)

func TestCopIgnoreRequested(t *testing.T) {
	t.Run("zero pieces never ignores requested", func(t *testing.T) {
		c := &chunks{chunkstate: chunkstate{pieces: 0, completed: roaring.New()}}
		assert.False(t, copIgnoreRequested(c))
	})

	t.Run("nothing completed ignores requested", func(t *testing.T) {
		completed := roaring.New()
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed}}
		assert.True(t, copIgnoreRequested(c))
	})

	t.Run("more than 5 percent remaining ignores requested", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 90)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed}}
		assert.True(t, copIgnoreRequested(c))
	})

	t.Run("5 percent or less remaining allows duplicate requests", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 95)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed}}
		assert.False(t, copIgnoreRequested(c))
	})

	t.Run("fully completed allows duplicate requests", func(t *testing.T) {
		completed := roaring.New()
		completed.AddRange(0, 100)
		c := &chunks{chunkstate: chunkstate{pieces: 100, completed: completed}}
		assert.False(t, copIgnoreRequested(c))
	})
}
