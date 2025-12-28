package torrenttestx

import (
	"fmt"
	"log"
	"net"
	"testing"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/internal/utpx"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func Autosocket(t *testing.T) torrent.Binder {
	var (
		bindings []sockets.Socket
	)
	_dht, err := dht.NewServer(32)
	require.NoError(t, err)

	s, err := utpx.New("udp4", "localhost:")
	require.NoError(t, err)
	bindings = append(bindings, sockets.New(s, s))

	if addr, ok := s.Addr().(*net.UDPAddr); ok {
		s, err := net.Listen("tcp4", fmt.Sprintf("localhost:%d", addr.Port))
		require.NoError(t, err)
		bindings = append(bindings, sockets.New(s, &net.Dialer{}))
	}

	return torrent.NewSocketsBind(bindings...).Options(
		torrent.BinderOptionDHT(_dht),
	)
}

func QuickClient(t testing.TB, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()
	return Client(t, torrent.NewMetadataCache(cdir), storage.NewFile(cdir), options...)
}

func Client(t testing.TB, mdcache torrent.MetadataStore, scache storage.ClientImpl, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()
	dht, err := dht.NewServer(32)
	require.NoError(t, err)

	return testx.Must(autobind.NewLoopback(autobind.EnableDHT(dht)).Bind(
		torrent.NewClient(
			torrent.NewDefaultClientConfig(
				mdcache,
				scache,
				torrent.ClientConfigCacheDirectory(cdir),
				// torrent.ClientConfigPeerID(krpc.RandomID().String()),
				torrent.ClientConfigSeed(true),
				torrent.ClientConfigInfoLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigDebugLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigDialPoolSize(1),
				torrent.ClientConfigUploadLimit(rate.NewLimiter(rate.Inf, 10)),
				torrent.ClientConfigCompose(options...),
			),
		),
	))(t)
}
