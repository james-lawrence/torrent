package int160_test

import (
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSecure(t *testing.T) {
	for _, tc := range []struct {
		ipStr     string
		nodeIDHex string
		valid     bool
	}{
		// Spec examples — all valid.
		{"124.31.75.21", "5fbfbff10c5d6a4ec8a88e4c6ab4c28b95eee401", true},
		{"21.75.31.124", "5a3ce9c14e7a08645677bbd1cfe7d8f956d53256", true},
		{"65.23.51.170", "a5d43220bc8f112a3d426c84764f8c2a1150e616", true},
		{"84.124.73.14", "1b0321dd1bb1fe518101ceef99462b947a01ff41", true},
		{"43.213.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51305a", true},
		// spec[0] with one random byte changed — still valid.
		{"124.31.75.21", "5fbfbff10c5d7a4ec8a88e4c6ab4c28b95eee401", true},
		// spec[1] with the 21st leading bit changed — not valid.
		{"21.75.31.124", "5a3ce1c14e7a08645677bbd1cfe7d8f956d53256", false},
		// spec[2] with the 22nd leading bit changed — valid.
		{"65.23.51.170", "a5d43620bc8f112a3d426c84764f8c2a1150e616", true},
		// spec[3] with the 4th-last bit changed — valid.
		{"84.124.73.14", "1b0321dd1bb1fe518101ceef99462b947a01fe01", true},
		// spec[4] with the 3rd-last bit changed — not valid.
		{"43.213.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51303e", false},
		// Class A — always secure.
		{"10.213.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51305a", true},
		// Not class A, id[0]&3 does not match — not valid.
		{"12.213.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51305a", false},
		// Class C — always secure.
		{"192.168.53.83", "e56f6cbf5b7c4be0237986d5243b87aa6d51305a", true},
	} {
		addr := netip.MustParseAddr(tc.ipStr)
		b, err := hex.DecodeString(tc.nodeIDHex)
		require.NoError(t, err)
		var arr [20]byte
		copy(arr[:], b)
		id := int160.FromByteArray(arr)

		secure := id.IsSecure(addr)
		assert.Equal(t, tc.valid, secure, "%v", tc)
		if !secure {
			secured := id.Secure(addr)
			assert.True(t, secured.IsSecure(addr), "%v", tc)
		}
	}
}

func TestIsSecureMultipleIPs(t *testing.T) {
	id := int160.Random()

	getInsecureAddr := func(size int) netip.Addr {
		buf := make([]byte, size)
		for {
			_, _ = rand.Read(buf)
			var addr netip.Addr
			if size == 4 {
				addr = netip.AddrFrom4([4]byte(buf))
			} else {
				addr = netip.AddrFrom16([16]byte(buf))
			}
			if !id.IsSecure(addr) {
				return addr
			}
		}
	}

	addr4 := getInsecureAddr(4)
	addr6 := getInsecureAddr(16)

	require.False(t, id.IsSecure(addr4))
	require.False(t, id.IsSecure(addr6))

	id4 := id.Secure(addr4)
	assert.True(t, id4.IsSecure(addr4))
	assert.False(t, id4.IsSecure(addr6))

	id6 := id.Secure(addr6)
	assert.True(t, id6.IsSecure(addr6))
	assert.False(t, id6.IsSecure(addr4))
}
