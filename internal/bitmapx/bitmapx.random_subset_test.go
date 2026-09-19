package bitmapx_test

import (
	"math/rand/v2"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/bitmapx"
)

func TestRandomSubset(t *testing.T) {
	t.Run("returns exactly the requested number of values from the bitmap", func(t *testing.T) {
		m := roaring.BitmapOf(3, 5, 8, 13, 21, 34, 55, 89, 144)

		subset := bitmapx.RandomSubsetFromSource(m, 4, rand.NewPCG(1, 2))

		require.EqualValues(t, 4, subset.GetCardinality())
		require.True(t, m.Contains(subset.Minimum()))
		require.True(t, roaring.And(subset, m).Equals(subset), "every value must come from the bitmap")
	})

	t.Run("is deterministic for a source", func(t *testing.T) {
		m := bitmapx.Fill(1000)

		a := bitmapx.RandomSubsetFromSource(m, 10, rand.NewPCG(1, 2))
		b := bitmapx.RandomSubsetFromSource(m, 10, rand.NewPCG(1, 2))

		require.Equal(t, a.ToArray(), b.ToArray())
	})

	t.Run("returns a copy of every value when the request is at least the cardinality", func(t *testing.T) {
		m := roaring.BitmapOf(2, 4, 6)

		for _, bits := range []uint64{3, 4, 100} {
			subset := bitmapx.RandomSubset(m, bits)
			require.Equal(t, []uint32{2, 4, 6}, subset.ToArray())

			// the subset is independent of the source bitmap.
			subset.Add(7)
			require.False(t, m.Contains(7))
		}
	})

	t.Run("returns an empty bitmap for an empty bitmap or zero values", func(t *testing.T) {
		require.True(t, bitmapx.RandomSubset(roaring.New(), 5).IsEmpty())
		require.True(t, bitmapx.RandomSubset(roaring.BitmapOf(1, 2, 3), 0).IsEmpty())
	})

	t.Run("does not modify the source bitmap", func(t *testing.T) {
		m := bitmapx.Fill(100)

		bitmapx.RandomSubset(m, 10)

		require.EqualValues(t, 100, m.GetCardinality())
	})
}
