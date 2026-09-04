package int160_test

import (
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func TestRangerFixed(t *testing.T) {
	t.Run("Generate always returns the configured value", func(t *testing.T) {
		expected := int160.Random()
		r := int160.NewRangeFixed(expected)

		require.Equal(t, expected, r.Generate())
		require.Equal(t, expected, r.Generate())
		require.Equal(t, expected, r.Generate())
	})

	t.Run("Generate returns the zero value when configured with zero", func(t *testing.T) {
		r := int160.NewRangeFixed(int160.Zero())

		require.Equal(t, int160.Zero(), r.Generate())
	})
}
