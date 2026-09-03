package torrent

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
)

func TestHashPieceAfterStorageClosed(t *testing.T) {
	td, err := os.MkdirTemp("", "")
	require.NoError(t, err)
	defer os.RemoveAll(td)
	store := storage.NewFile(td)
	tt := newTorrent(&Client{config: &ClientConfig{}}, Metadata{Storage: store})

	mi := testutil.GreetingMetaInfo()
	info, err := mi.UnmarshalInfo()
	require.NoError(t, err)
	require.NoError(t, tt.setInfo(&info))
	require.NoError(t, tt.storage.Close())
	tt.digests.Enqueue(0)
}

func TestDigestFailureDoesNotInflateBytesValidated(t *testing.T) {
	td, err := os.MkdirTemp("", "")
	require.NoError(t, err)
	defer os.RemoveAll(td)
	store := storage.NewFile(td)
	tt := newTorrent(&Client{config: &ClientConfig{}}, Metadata{Storage: store})

	const pieceLength = 5
	info, _, err := torrenttest.Seeded(t.TempDir(), 13, cryptox.NewChaCha8(t.Name()), metainfo.OptionPieceLength(pieceLength))
	require.NoError(t, err)
	require.NoError(t, tt.setInfo(info))
	// setInfo (unlike setInfoBytes) does not refresh digests' storage reader,
	// so it's still bound to the placeholder captured at torrent construction.
	// Mirror what setInfoBytes does for the metadata-exchange path.
	*tt.digests = newDigestsFromTorrent(tt)

	correct := make([]byte, pieceLength)
	_, err = io.ReadFull(cryptox.NewChaCha8(t.Name()), correct)
	require.NoError(t, err)

	// corrupt piece 0 and confirm a failed digest never counts toward BytesValidated.
	wrong := append([]byte(nil), correct...)
	wrong[0] ^= 0xff
	require.NoError(t, tt.writeChunk(0, 0, wrong))
	tt.digests.Enqueue(0)
	tt.digests.Wait()
	require.Equal(t, int64(0), tt.stats.BytesValidated.Int64(), "a failed digest must never count toward BytesValidated")

	// now write the correct bytes and confirm it validates exactly once, for
	// exactly the piece's real length.
	require.NoError(t, tt.writeChunk(0, 0, correct))
	tt.digests.Enqueue(0)
	tt.digests.Wait()
	require.Equal(t, tt.info.Piece(0).Length(), tt.stats.BytesValidated.Int64(), "a successful digest must count exactly the piece's real length, once")

	// a piece can legitimately be digest-checked more than once for the same
	// completed data - e.g. BitTorrent endgame mode racing two peer
	// connections to deliver the same last chunk can enqueue the same piece
	// index twice. a redundant re-check of already-complete data must not
	// double-count BytesValidated.
	tt.digests.Enqueue(0)
	tt.digests.Wait()
	require.Equal(t, tt.info.Piece(0).Length(), tt.stats.BytesValidated.Int64(), "re-checking an already-complete piece must not double-count BytesValidated")
}
