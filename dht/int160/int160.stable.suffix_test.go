package int160_test

import (
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/require"
)

func TestStableSuffix(t *testing.T) {
	id := int160.Random()
	s := int160.StableSuffix(id)
	sArr, idArr := s.AsByteArray(), id.AsByteArray()
	require.Equal(t, [3]byte{}, [3]byte(sArr[:3]))
	require.Equal(t, idArr[3:], sArr[3:])
}
