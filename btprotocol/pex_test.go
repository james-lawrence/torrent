package btprotocol

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/bencode"
)

func TestUnmarshalPex(t *testing.T) {
	var pem PexMsg
	err := bencode.Unmarshal([]byte("d5:added12:\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0ce"), &pem)
	require.NoError(t, err)
	require.EqualValues(t, 2, len(pem.Added))
	require.EqualValues(t, 1286, pem.Added[0].Port())
	require.EqualValues(t, 0x100*0xb+0xc, pem.Added[1].Port())
}

func TestEmptyPexMsg(t *testing.T) {
	pm := PexMsg{}
	b, err := bencode.Marshal(pm)
	t.Logf("%q", b)
	require.NoError(t, err)
	require.NoError(t, bencode.Unmarshal(b, &pm))
}
