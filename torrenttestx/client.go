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

// bindMatchingTCP binds a TCP listener on the same port number as the
// already-bound uTP socket u. There's no atomicity between the two binds -
// the uTP socket's ephemeral port can be reused by another process/goroutine
// (e.g. a just-finished test iteration's lingering listener) between when
// the OS assigns it and when we try to claim the matching TCP port - so this
// retries on a bind failure with a fresh uTP+TCP pair rather than failing
// (or, if the race resolves silently instead of erroring, wiring up peers
// that never actually exchange data) outright.
func bindMatchingTCP(t *testing.T) (utpx.Socket, net.Listener) {
	const maxattempts = 10

	for attempt := 1; ; attempt++ {
		u, err := utpx.New("udp4", "localhost:")
		require.NoError(t, err)

		addr, ok := u.Addr().(*net.UDPAddr)
		if !ok {
			return u, nil
		}

		l, err := net.Listen("tcp4", fmt.Sprintf("localhost:%d", addr.Port))
		if err == nil {
			return u, l
		}

		u.Close()
		if attempt == maxattempts {
			require.NoError(t, err)
		}
	}
}

func Autosocket(t *testing.T) torrent.Binder {
	var (
		bindings []sockets.Socket
	)
	_dht, err := dht.NewServer(32)
	require.NoError(t, err)

	u, l := bindMatchingTCP(t)
	bindings = append(bindings, sockets.New(u, u))
	if l != nil {
		bindings = append(bindings, sockets.New(l, &net.Dialer{}))
	}

	return torrent.NewSocketsBind(bindings...).Options(
		torrent.BinderOptionDHT(_dht),
	)
}

func QuickDHT(t testing.TB, options ...dht.Option) *dht.Server {
	dhts, err := dht.NewServer(
		32,
		options...,
	)
	require.NoError(t, err)
	return dhts
}

func QuickClient(t testing.TB, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()
	return Client(t, autobind.NewLoopback(
		autobind.EnableDHT(QuickDHT(t)), // dht should be optional, but refactor made it not for now default to a basic one
	), torrent.NewMetadataCache(cdir), storage.NewFile(cdir), options...)
}

func QuickClientWithDHT(t testing.TB, dhts *dht.Server, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()
	return Client(t, autobind.NewLoopback(autobind.EnableDHT(dhts)), torrent.NewMetadataCache(cdir), storage.NewFile(cdir), options...)
}

func Client(t testing.TB, binder autobind.Autobind, mdcache torrent.MetadataStore, scache storage.ClientImpl, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()

	return testx.Must(binder.Bind(
		torrent.NewClient(
			torrent.NewDefaultClientConfig(
				mdcache,
				scache,
				torrent.ClientConfigCacheDirectory(cdir),
				torrent.ClientConfigSeed(true),
				torrent.ClientConfigInfoLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigDebugLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigCompose(options...),
				torrent.ClientConfigDialPoolSize(1),
				torrent.ClientConfigUploadLimit(rate.NewLimiter(rate.Inf, 10)),
			),
		),
	))(t)
}
