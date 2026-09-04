package int160_test

import (
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func TestRangerRandom(t *testing.T) {
	t.Run("Generate returns a non-zero value", func(t *testing.T) {
		r := int160.NewRangeRandom()
		require.NotEqual(t, int160.Zero(), r.Generate())
	})

	t.Run("successive calls produce distinct values", func(t *testing.T) {
		r := int160.NewRangeRandom()
		require.NotEqual(t, r.Generate(), r.Generate())
	})
}
