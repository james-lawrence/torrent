package torrent

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/storage"
)

func TestHashPieceAfterStorageClosed(t *testing.T) {
	td, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(td)
	tt := newTorrent(&Client{config: &ClientConfig{}}, Metadata{Storage: storage.NewFile(td)})
	tt.digests = newDigests(
		func(idx int) *Piece { return tt.piece(idx) },
		func(idx int, cause error) {},
	)

	mi := testutil.GreetingMetaInfo()
	info, err := mi.UnmarshalInfo()
	require.NoError(t, err)
	require.NoError(t, tt.setInfo(&info))
	require.NoError(t, tt.storage.Close())
	tt.digests.Enqueue(0)
}
