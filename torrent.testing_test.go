package torrent_test

import (
	"hash"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/storage"
	"github.com/stretchr/testify/require"
)

func TorrentFile(t testing.TB, dir string, src io.Reader, n int64) (d torrent.Metadata, _ hash.Hash) {
	data, expected, err := testutil.IOTorrent(dir, cryptox.NewChaCha8(t.Name()), n)
	require.NoError(t, err)
	defer os.Remove(data.Name())

	metadata, err := torrent.NewFromFile(
		data.Name(),
		torrent.OptionStorage(storage.NewFile(dir, storage.FileOptionPathMakerFixed(filepath.Base(data.Name())))),
	)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadata.ID.String()), 0700))
	return metadata, expected
}
