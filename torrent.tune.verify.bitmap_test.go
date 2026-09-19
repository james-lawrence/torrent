package torrent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
)

// the torrent has 32 pieces of 256KiB, every piece is a full piece, with 16KiB chunks that is 16 chunks per piece.
// n is 0 in most of the tests so only the first and last downloaded pieces are sampled, which makes the result deterministic.
func TestTuneVerifyBitmap(t *testing.T) {
	const (
		pieceLength int64  = 256 * bytesx.KiB
		pieces      uint64 = 32
	)

	dir := t.TempDir()
	info, _, err := torrenttest.Random(dir, uint64(pieceLength)*pieces, metainfo.OptionPieceLength(pieceLength))
	require.NoError(t, err)

	cl, err := Autosocket(t).Bind(NewClient(TestingConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer cl.Close()

	md, err := NewFromInfo(info, OptionStorage(storage.NewFile(dir)))
	require.NoError(t, err)

	dl, _, err := cl.Start(md)
	require.NoError(t, err)
	tt := dl.(*torrent)

	require.EqualValues(t, pieces, tt.chunks.pieces)
	cpp := uint64(chunksPerPiece(pieceLength, tt.chunks.clength))
	// the test torrent places its data in a file named after the torrent's hash.
	path := filepath.Join(dir, md.ID.String())

	t.Run("verifies the first and last downloaded pieces and leaves the rest unverified", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Fill(uint64(tt.chunks.cmaximum)), 0)))

		require.Equal(t, []uint32{0, uint32(pieces - 1)}, tt.chunks.completed.ToArray())
		require.Equal(t, bitmapx.Range(cpp, (pieces-1)*cpp).ToArray(), tt.chunks.unverified.ToArray())
		require.True(t, tt.chunks.missing.IsEmpty())
		require.True(t, tt.chunks.failed.IsEmpty())
		// only the sampled pieces are hashed, the rest are credited as validated since the bitmap was correct.
		require.EqualValues(t, pieces*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
		require.Equal(t, bitmapx.Range(1, pieces-1).ToArray(), tt.chunks.credited.ToArray())
	})

	t.Run("does not count the credited pieces again when they are hashed later", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Fill(uint64(tt.chunks.cmaximum)), 0)))
		before := tt.stats.BytesValidated.Uint64()

		// hash everything that wasn't sampled, this is what the reader does as it reads the pieces.
		tt.digests.EnqueueBitmap(bitmapx.Range(1, pieces-1))
		tt.digests.Wait()

		require.Equal(t, bitmapx.Fill(pieces).ToArray(), tt.chunks.completed.ToArray())
		require.True(t, tt.chunks.unverified.IsEmpty())
		require.True(t, tt.chunks.credited.IsEmpty())
		require.EqualValues(t, 0, tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("counts a piece once when it is credited and hashed again after a reset", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Fill(uint64(tt.chunks.cmaximum)), 0)))
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		require.NoError(t, tt.Tune(TuneVerifyFull))

		require.EqualValues(t, pieces*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("verifies every downloaded piece when n covers them all", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Fill(uint64(tt.chunks.cmaximum)), pieces)))

		require.Equal(t, bitmapx.Fill(pieces).ToArray(), tt.chunks.completed.ToArray())
		require.True(t, tt.chunks.unverified.IsEmpty())
		require.True(t, tt.chunks.missing.IsEmpty())
		require.EqualValues(t, pieces*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("only samples the pieces that were downloaded", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		// the first 10 pieces are downloaded.
		const downloaded = 10
		_, last := tt.chunks.Range(downloaded - 1)
		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Range(0, last), 0)))

		require.Equal(t, []uint32{0, downloaded - 1}, tt.chunks.completed.ToArray())
		require.Equal(t, bitmapx.Range(cpp, (downloaded-1)*cpp).ToArray(), tt.chunks.unverified.ToArray())
		require.EqualValues(t, uint64(tt.chunks.cmaximum)-last, tt.chunks.missing.GetCardinality())
		require.False(t, tt.chunks.missing.Contains(uint32(last-1)))
		require.True(t, tt.chunks.missing.Contains(uint32(last)))
		// the sampled pieces are hashed and the rest of the downloaded pieces are credited.
		require.EqualValues(t, downloaded*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("leaves partially downloaded pieces unverified", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		// pieces 0 and 1 are downloaded, piece 2 only has its first 3 chunks.
		snapshot := bitmapx.Range(0, 2*cpp)
		snapshot.AddRange(2*cpp, 2*cpp+3)
		require.NoError(t, tt.Tune(TuneVerifyBitmap(snapshot, 4)))

		require.Equal(t, []uint32{0, 1}, tt.chunks.completed.ToArray())
		require.Equal(t, []uint32{uint32(2 * cpp), uint32(2*cpp + 1), uint32(2*cpp + 2)}, tt.chunks.unverified.ToArray())
		require.EqualValues(t, uint64(tt.chunks.cmaximum)-(2*cpp+3), tt.chunks.missing.GetCardinality())
		require.EqualValues(t, 2*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("assumes everything was downloaded when there is no snapshot and the data is on disk", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Range(0, 0), 0)))

		require.Equal(t, []uint32{0, uint32(pieces - 1)}, tt.chunks.completed.ToArray())
		require.Equal(t, bitmapx.Range(cpp, (pieces-1)*cpp).ToArray(), tt.chunks.unverified.ToArray())
		require.True(t, tt.chunks.missing.IsEmpty())
		require.True(t, tt.chunks.failed.IsEmpty())
		require.EqualValues(t, pieces*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("marks everything missing when there is no snapshot and nothing is on disk", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))
		before := tt.stats.BytesValidated.Uint64()

		original, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, make([]byte, len(original)), 0600))
		t.Cleanup(func() {
			require.NoError(t, os.WriteFile(path, original, 0600))
		})

		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Range(0, 0), 8)))

		require.True(t, tt.chunks.completed.IsEmpty())
		require.True(t, tt.chunks.unverified.IsEmpty())
		require.True(t, tt.chunks.failed.IsEmpty())
		require.EqualValues(t, tt.chunks.cmaximum, tt.chunks.missing.GetCardinality())
		require.EqualValues(t, 0, tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("verifies the entire torrent when the first downloaded piece fails", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))

		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		t.Cleanup(func() { f.Close() })

		// downloaded pieces are 5 to 9, piece 5 is sampled and corrupt. the bitmap is incorrect so everything is
		// verified, piece 20 is corrupt as well and is only found because everything is verified.
		for _, pid := range []int64{5, 20} {
			original := make([]byte, 16)
			_, err = f.ReadAt(original, pid*pieceLength+5)
			require.NoError(t, err)
			_, err = f.WriteAt(make([]byte, 16), pid*pieceLength+5)
			require.NoError(t, err)

			t.Cleanup(func() {
				_, err := f.WriteAt(original, pid*pieceLength+5)
				require.NoError(t, err)
			})
		}

		_, last := tt.chunks.Range(9)
		snapshot := bitmapx.Range(5*cpp, last)
		before := tt.stats.BytesValidated.Uint64()
		require.NoError(t, tt.Tune(TuneVerifyBitmap(snapshot, 0)))

		expected := bitmapx.Fill(pieces)
		expected.Remove(5)
		expected.Remove(20)
		require.Equal(t, expected.ToArray(), tt.chunks.completed.ToArray())

		failed := bitmapx.Range(5*cpp, 6*cpp)
		failed.AddRange(20*cpp, 21*cpp)
		require.Equal(t, failed.ToArray(), tt.chunks.missing.ToArray())
		require.True(t, tt.chunks.unverified.IsEmpty())
		require.True(t, tt.chunks.failed.IsEmpty())
		require.EqualValues(t, (pieces-2)*uint64(pieceLength), tt.stats.BytesValidated.Uint64()-before)
	})

	t.Run("verifies the entire torrent when the last downloaded piece fails", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))

		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		t.Cleanup(func() { f.Close() })

		// downloaded pieces are 5 to 9, piece 9 is sampled and corrupt.
		offset := 9*pieceLength + 5
		original := make([]byte, 16)
		_, err = f.ReadAt(original, offset)
		require.NoError(t, err)
		_, err = f.WriteAt(make([]byte, 16), offset)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := f.WriteAt(original, offset)
			require.NoError(t, err)
		})

		_, last := tt.chunks.Range(9)
		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Range(5*cpp, last), 0)))

		expected := bitmapx.Fill(pieces)
		expected.Remove(9)
		require.Equal(t, expected.ToArray(), tt.chunks.completed.ToArray())
		require.Equal(t, bitmapx.Range(9*cpp, 10*cpp).ToArray(), tt.chunks.missing.ToArray())
		require.True(t, tt.chunks.unverified.IsEmpty())
	})

	t.Run("does not find corruption in pieces that were not sampled", func(t *testing.T) {
		require.NoError(t, tt.Tune(TuneResetBitmaps))

		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		t.Cleanup(func() { f.Close() })

		// only the first and last pieces are sampled, the corruption is found later when the piece is read.
		offset := 20*pieceLength + 5
		original := make([]byte, 16)
		_, err = f.ReadAt(original, offset)
		require.NoError(t, err)
		_, err = f.WriteAt(make([]byte, 16), offset)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := f.WriteAt(original, offset)
			require.NoError(t, err)
		})

		require.NoError(t, tt.Tune(TuneVerifyBitmap(bitmapx.Fill(uint64(tt.chunks.cmaximum)), 0)))

		require.Equal(t, []uint32{0, uint32(pieces - 1)}, tt.chunks.completed.ToArray())
		require.True(t, tt.chunks.unverified.Contains(uint32(20*cpp)))
		require.True(t, tt.chunks.missing.IsEmpty())
		require.True(t, tt.chunks.failed.IsEmpty())
	})
}
