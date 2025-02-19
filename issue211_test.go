package torrent_test

import (
	"io"
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/storage"
)

func TestDropTorrentWithMmapStorageWhileHashing(t *testing.T) {
	cfg := torrent.TestingConfig(t)
	// Ensure the data is present when the torrent is added, and not obtained
	// over the network as the test runs.
	cfg.DownloadRateLimiter = rate.NewLimiter(0, 0)
	cl, err := torrent.NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()

	td, mi := testutil.GreetingTestTorrent(t)
	ts, err := torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewMMap(td)))
	require.NoError(t, err)
	tt, new, err := cl.Start(ts)
	require.NoError(t, err)
	assert.True(t, new)

	r := tt.NewReader()
	go cl.Stop(ts)
	io.Copy(ioutil.Discard, r)
}
