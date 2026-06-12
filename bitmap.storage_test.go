package torrent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func TestBitmapCache(t *testing.T) {
	t.Run("discards corrupted bitmap", func(t *testing.T) {
		// build a serialized roaring bitmap containing a single array
		// container whose contents are out of order. ReadFrom reads the
		// container bytes verbatim without checking the invariant, so this
		// deserializes "successfully" into a bitmap that fails Validate() -
		// the same class of corruption (passes deserialization, fails the
		// container's internal invariants) that leads to the
		// "index out of range [4096] with length 4096" panic in
		// bitmapContainer.fillArray during CheckedRemove/toArrayContainer.
		src := roaring.New()
		src.Add(1)
		src.Add(2)
		src.Add(3)

		var buf bytes.Buffer
		_, err := src.WriteTo(&buf)
		require.NoError(t, err)

		data := buf.Bytes()

		// header (no run containers, 1 container): cookie(4) + size(4) + key(2) + (cardinality-1)(2) + offset(4) = 16
		const headerSize = 16
		// swap the first two array entries (1, 2) -> (2, 1), breaking the
		// required ascending order.
		data[headerSize], data[headerSize+2] = data[headerSize+2], data[headerSize]

		root := t.TempDir()
		id := int160.Random()
		require.NoError(t, os.WriteFile(filepath.Join(root, id.String()+".bitmap"), data, 0600))

		store := NewBitmapCache(root)
		bm, err := store.Read(id)
		require.NoError(t, err)
		require.NoError(t, bm.Validate(), "Read should not return a corrupted bitmap")
	})
}
