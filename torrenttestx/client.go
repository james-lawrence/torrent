package torrenttestx

import (
	"log"
	"testing"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"golang.org/x/time/rate"
)

func QuickClient(t testing.TB, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()
	return Client(t, torrent.NewMetadataCache(cdir), storage.NewFile(cdir), options...)
}

func Client(t testing.TB, mdcache torrent.MetadataStore, scache storage.ClientImpl, options ...torrent.ClientConfigOption) *torrent.Client {
	cdir := t.TempDir()

	return testx.Must(autobind.NewLoopback().Bind(
		torrent.NewClient(
			torrent.NewDefaultClientConfig(
				mdcache,
				scache,
				torrent.ClientConfigCacheDirectory(cdir),
				torrent.ClientConfigPeerID(krpc.RandomID().String()),
				torrent.ClientConfigSeed(true),
				torrent.ClientConfigInfoLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigDebugLogger(log.New(log.Writer(), "[torrent] ", log.Flags())),
				torrent.ClientConfigMuxer(dht.DefaultMuxer()),
				torrent.ClientConfigBucketLimit(32),
				torrent.ClientConfigCompose(options...),
				torrent.ClientConfigDialPoolSize(1),
				torrent.ClientConfigUploadLimit(rate.NewLimiter(rate.Inf, 10)),
			),
		),
	))(t)
}
