package int160_test

import (
	"encoding/hex"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecure(t *testing.T) {
	for _, tc := range []struct {
		ipStr     string
		nodeIDHex string
	}{
		{"124.31.75.21", "5fbfbff10c5d6a4ec8a88e4c6ab4c28b95eee401"},
		{"21.75.31.124", "5a3ce9c14e7a08645677bbd1cfe7d8f956d53256"},
		{"65.23.51.170", "a5d43220bc8f112a3d426c84764f8c2a1150e616"},
		{"84.124.73.14", "1b0321dd1bb1fe518101ceef99462b947a01ff41"},
		{"43.213.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51305a"},
		{"141.154.115.212", "295a1062f24dcbece65e7be063198bd6b3eaf17a"},
		{"2600:4040:5251:7000::b939:ea69", "00000062f24dcbece65e7be063198bd6b3eaf17a"},
		{"2600:4040:5251:7000::b939:ea69", "aa91d062f24dcbece65e7be063198bd6b3eaf17a"},
	} {
		addr := netip.MustParseAddr(tc.ipStr)
		b, err := hex.DecodeString(tc.nodeIDHex)
		require.NoError(t, err, "ip(%s) - id(%s)", tc.ipStr, tc.nodeIDHex)
		var arr [20]byte
		copy(arr[:], b)
		id := int160.FromByteArray(arr)

		secured := id.Secure(addr)
		assert.True(t, secured.IsSecure(addr), "secured ID should validate for %s", tc.ipStr)
	}
}

func TestSecurePreservesStableSuffix(t *testing.T) {
	id := int160.Random()
	addr := netip.MustParseAddr("1.2.3.4")
	secured := id.Secure(addr)
	idArr, securedArr := id.AsByteArray(), secured.AsByteArray()
	require.Equal(t, idArr[3:], securedArr[3:])
}
