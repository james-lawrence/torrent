package torrent

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/utpx"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, options ...ClientConfigOption) *Client {
	dir := t.TempDir()
	cl, err := NewClient(NewDefaultClientConfig(NewMetadataCache(dir), storage.NewFile(dir), append(options, ClientConfigCacheDirectory(t.TempDir()))...))
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

func TestClientAlwaysHasDHTIdentity(t *testing.T) {
	cl := newTestClient(t)

	require.NotNil(t, cl.dht)
	require.False(t, cl.dht.Available())

	// the client must be able to tell peers apart from itself.
	require.NotEqual(t, int160.Zero(), cl.dht.ID(netip.MustParseAddrPort("127.0.0.1:2000")))
}

func TestBindDHTReplaces(t *testing.T) {
	cl := newTestClient(t)
	placeholder := cl.dht

	id := int160.Random()
	real, err := dht.NewServer(32, dht.OptionNodeID(id))
	require.NoError(t, err)
	defer real.Close()

	cl.BindDHT(real)
	require.Same(t, real, cl.dht)
	require.NotSame(t, placeholder, cl.dht)
	require.Equal(t, id, cl.dht.ID(netip.MustParseAddrPort("127.0.0.1:2000")))

	// nil is ignored.
	cl.BindDHT(nil)
	require.Same(t, real, cl.dht)
}

func TestBindServesPacketSockets(t *testing.T) {
	cl := newTestClient(t)

	u, err := utpx.New("udp4", "localhost:")
	require.NoError(t, err)
	defer u.Close()

	require.False(t, cl.dht.Available())
	require.NoError(t, cl.Bind(sockets.New(u, u)))
	require.True(t, cl.dht.Available())
}
