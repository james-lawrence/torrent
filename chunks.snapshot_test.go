package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/bitmapx"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/metainfo"
)

// the tests use a 16KiB torrent with 1KiB pieces and 256 byte chunks: 16 pieces of 4 chunks each.
func TestDownloadedSnapshot(t *testing.T) {
	t.Run("restore recreates the unverified and missing chunks that were saved", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())
		require.EqualValues(t, 64, a.cmaximum)

		a.unverified.AddMany([]uint32{0, 1, 2, 9, 20, 21})
		a.missing = bitmapx.AndNot(bitmapx.Fill(uint64(a.cmaximum)), a.unverified)

		snapshot := DownloadedSnapshotSave(a)
		require.Equal(t, []uint32{0, 1, 2, 9, 20, 21}, snapshot.ToArray())

		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)

		require.Equal(t, a.unverified.ToArray(), b.unverified.ToArray())
		require.Equal(t, a.missing.ToArray(), b.missing.ToArray())
		require.True(t, b.completed.IsEmpty())
	})

	t.Run("restore leaves the chunks of completed pieces unverified", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())

		// pieces 0 and 2 are complete, piece 1 has chunks 5 and 6 of (4, 5, 6, 7).
		a.completed.AddMany([]uint32{0, 2})
		a.unverified.AddMany([]uint32{5, 6})
		downloaded := bitmapx.Range(0, 4)
		downloaded.AddRange(8, 12)
		downloaded.AddMany([]uint32{5, 6})
		a.missing = bitmapx.AndNot(bitmapx.Fill(uint64(a.cmaximum)), downloaded)

		snapshot := DownloadedSnapshotSave(a)
		require.Equal(t, []uint32{0, 1, 2, 3, 5, 6, 8, 9, 10, 11}, snapshot.ToArray())

		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)

		require.Equal(t, []uint32{0, 1, 2, 3, 5, 6, 8, 9, 10, 11}, b.unverified.ToArray())
		require.True(t, b.completed.IsEmpty())
		require.Equal(t, a.missing.ToArray(), b.missing.ToArray())

		// saving the restored chunks gives back the same snapshot.
		require.Equal(t, snapshot.ToArray(), DownloadedSnapshotSave(b).ToArray())
	})

	t.Run("restore recreates the state of chunks with nothing downloaded", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())
		a.missing = bitmapx.Fill(uint64(a.cmaximum))

		snapshot := DownloadedSnapshotSave(a)
		require.True(t, snapshot.IsEmpty())

		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)

		require.True(t, b.unverified.IsEmpty())
		require.True(t, b.completed.IsEmpty())
		require.EqualValues(t, a.cmaximum, b.missing.GetCardinality())
		require.Equal(t, a.missing.ToArray(), b.missing.ToArray())
	})

	t.Run("restore leaves every chunk unverified when every piece was completed", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())
		a.completed = bitmapx.Fill(a.pieces)

		snapshot := DownloadedSnapshotSave(a)
		require.EqualValues(t, a.cmaximum, snapshot.GetCardinality())

		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)

		require.EqualValues(t, a.cmaximum, b.unverified.GetCardinality())
		require.True(t, b.missing.IsEmpty())
		require.True(t, b.completed.IsEmpty())
	})

	t.Run("restore handles a final piece that is shorter than the others", func(t *testing.T) {
		// 10.5 KiB: 10 full pieces (chunks 0-39) and a final piece of 2 chunks (40, 41).
		info := func() *metainfo.Info {
			return &metainfo.Info{
				Length:      10*bytesx.KiB + 512,
				PieceLength: bytesx.KiB,
				Pieces:      make([]byte, 20*11),
			}
		}

		a := newChunks(256, info())
		require.EqualValues(t, 42, a.cmaximum)
		require.EqualValues(t, 11, a.pieces)

		a.completed.AddMany([]uint32{0, 10})
		downloaded := bitmapx.Range(0, 4)
		downloaded.AddRange(40, 42)
		a.missing = bitmapx.AndNot(bitmapx.Fill(uint64(a.cmaximum)), downloaded)

		snapshot := DownloadedSnapshotSave(a)
		require.Equal(t, []uint32{0, 1, 2, 3, 40, 41}, snapshot.ToArray())

		b := newChunks(256, info())
		DownloadedSnapshotRestore(snapshot)(b)

		require.Equal(t, []uint32{0, 1, 2, 3, 40, 41}, b.unverified.ToArray())
		require.True(t, b.completed.IsEmpty())
		require.Equal(t, a.missing.ToArray(), b.missing.ToArray())
	})

	t.Run("restore ignores chunks outside of the torrent", func(t *testing.T) {
		snapshot := bitmapx.Range(0, 4)
		snapshot.AddMany([]uint32{6, 64, 65, 100000})

		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)

		require.Equal(t, []uint32{0, 1, 2, 3, 6}, b.unverified.ToArray())
		require.True(t, b.completed.IsEmpty())
		require.EqualValues(t, b.cmaximum-5, b.missing.GetCardinality())
		require.False(t, b.missing.Contains(64))
		require.False(t, b.unverified.Contains(64))
	})

	t.Run("restore replaces the existing state but leaves inflight and failed alone", func(t *testing.T) {
		b := newChunks(256, tinyTorrentInfo())
		b.unverified.AddMany([]uint32{50, 51})
		b.completed.Add(7)
		b.missing.AddMany([]uint32{1, 2, 3})
		b.inflight.Add(40)
		b.failed.Add(41)

		snapshot := bitmapx.Range(8, 12)
		DownloadedSnapshotRestore(snapshot)(b)

		require.Equal(t, []uint32{8, 9, 10, 11}, b.unverified.ToArray())
		require.True(t, b.completed.IsEmpty())
		require.EqualValues(t, b.cmaximum-4, b.missing.GetCardinality())
		require.False(t, b.missing.Contains(8))
		require.Equal(t, []uint32{40}, b.inflight.ToArray())
		require.Equal(t, []uint32{41}, b.failed.ToArray())
	})

	t.Run("save does not modify the chunks", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())
		a.completed.AddMany([]uint32{0, 2})
		a.unverified.AddMany([]uint32{5, 6})
		a.missing.AddMany([]uint32{20, 21})
		a.inflight.Add(22)
		a.failed.Add(23)

		completed, unverified, missing, inflight, failed := a.completed.Clone(), a.unverified.Clone(), a.missing.Clone(), a.inflight.Clone(), a.failed.Clone()

		DownloadedSnapshotSave(a)

		require.True(t, completed.Equals(a.completed))
		require.True(t, unverified.Equals(a.unverified))
		require.True(t, missing.Equals(a.missing))
		require.True(t, inflight.Equals(a.inflight))
		require.True(t, failed.Equals(a.failed))
	})

	t.Run("save returns a standalone bitmap", func(t *testing.T) {
		a := newChunks(256, tinyTorrentInfo())
		a.completed.Add(0)
		a.unverified.AddMany([]uint32{5, 6})

		snapshot := DownloadedSnapshotSave(a)
		expected := snapshot.Clone()

		// changing the chunks doesn't change the snapshot.
		a.unverified.Add(60)
		a.completed.Add(3)
		require.True(t, expected.Equals(snapshot))

		// changing the snapshot doesn't change the chunks.
		snapshot.Add(61)
		require.False(t, a.unverified.Contains(61))

		// changing the snapshot after a restore doesn't change the restored chunks.
		b := newChunks(256, tinyTorrentInfo())
		DownloadedSnapshotRestore(snapshot)(b)
		snapshot.Add(62)
		require.False(t, b.unverified.Contains(62))
		require.True(t, b.missing.Contains(62))
	})
}
