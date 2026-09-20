package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/metainfo"
)

// the tests use a 16KiB torrent with 1KiB pieces and 256 byte chunks: 16 pieces of 4 chunks each.
func TestCopSnapshot(t *testing.T) {
	t.Run("counts the chunks in each state", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.missing.AddMany([]uint32{30, 31, 32, 33, 34})
		c.inflight.AddMany([]uint32{20, 21, 22})
		c.unverified.AddMany([]uint32{5, 6})
		c.failed.Add(40)
		c.completed.AddMany([]uint32{0, 2, 3, 4})

		s := c.Read(copSnapshot(&Stats{}))
		require.Equal(t, 5, s.Missing)
		require.Equal(t, 3, s.Outstanding)
		require.Equal(t, 2, s.Unverified)
		require.Equal(t, 1, s.Failed)
		require.Equal(t, 4, s.Completed)
	})

	t.Run("nothing downloaded leaves every byte remaining", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 0, s.Downloaded)
		require.EqualValues(t, 0, s.DownloadedOptimistic)
		require.EqualValues(t, 16*bytesx.KiB, s.Remaining)
	})

	t.Run("completed pieces count as a full piece length", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.completed.AddMany([]uint32{0, 2})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 2*bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 14*bytesx.KiB, s.Remaining)
	})

	t.Run("credited pieces count as a full piece length", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.unverified.AddRange(4, 12)
		c.credited.AddMany([]uint32{1, 2})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 2*bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 14*bytesx.KiB, s.Remaining)
	})

	t.Run("a piece that is both completed and credited is only counted once", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.completed.AddMany([]uint32{0, 1})
		c.credited.AddMany([]uint32{1, 2})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 3*bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 13*bytesx.KiB, s.Remaining)
	})

	t.Run("unverified chunks are not downloaded", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.unverified.AddMany([]uint32{5, 6})
		c.completed.Add(0)

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 15*bytesx.KiB, s.Remaining)
	})

	t.Run("optimistic downloaded counts completed pieces and unverified chunks", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.completed.AddMany([]uint32{0, 2})
		c.unverified.AddMany([]uint32{5, 6})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, (2*bytesx.KiB)+(2*256), s.DownloadedOptimistic)
		require.EqualValues(t, 2*bytesx.KiB, s.Downloaded, "only the good pieces are downloaded")
	})

	t.Run("optimistic downloaded does not count credited pieces twice", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.unverified.AddRange(4, 12)
		c.credited.AddMany([]uint32{1, 2})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 8*256, s.DownloadedOptimistic)
		require.EqualValues(t, 2*bytesx.KiB, s.Downloaded)
	})

	t.Run("optimistic downloaded of a completed last piece shorter than the others only counts its real length", func(t *testing.T) {
		info := &metainfo.Info{
			Length:      10*bytesx.KiB + 512,
			PieceLength: bytesx.KiB,
			Pieces:      make([]byte, 20*11),
		}

		c := newChunks(256, info)
		c.completed.AddMany([]uint32{0, 10})
		c.unverified.AddMany([]uint32{8, 9})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, bytesx.KiB+512+(2*256), s.DownloadedOptimistic)
	})

	t.Run("outstanding missing and failed chunks are not downloaded", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.missing.AddMany([]uint32{30, 31})
		c.inflight.AddMany([]uint32{20, 21})
		c.failed.Add(40)

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 0, s.Downloaded)
		require.EqualValues(t, 16*bytesx.KiB, s.Remaining)
	})

	t.Run("a completed last piece of a torrent evenly divisible by the piece length is a full piece", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		require.EqualValues(t, 16, c.pieces)
		c.completed.Add(15)

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 15*bytesx.KiB, s.Remaining)
	})

	t.Run("a completed last piece shorter than the others only counts its real length", func(t *testing.T) {
		// 10.5 KiB: 10 full pieces and a final piece of 512 bytes.
		info := &metainfo.Info{
			Length:      10*bytesx.KiB + 512,
			PieceLength: bytesx.KiB,
			Pieces:      make([]byte, 20*11),
		}

		c := newChunks(256, info)
		require.EqualValues(t, 11, c.pieces)
		c.completed.AddMany([]uint32{0, 10})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, bytesx.KiB+512, s.Downloaded)
		require.EqualValues(t, 9*bytesx.KiB, s.Remaining)
	})

	t.Run("a credited last piece shorter than the others only counts its real length", func(t *testing.T) {
		info := &metainfo.Info{
			Length:      10*bytesx.KiB + 512,
			PieceLength: bytesx.KiB,
			Pieces:      make([]byte, 20*11),
		}

		c := newChunks(256, info)
		c.credited.AddMany([]uint32{0, 10})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, bytesx.KiB+512, s.Downloaded)
		require.EqualValues(t, 9*bytesx.KiB, s.Remaining)
	})

	t.Run("optimistic downloaded of a credited last piece is not adjusted since credited pieces are counted by their unverified chunks", func(t *testing.T) {
		info := &metainfo.Info{
			Length:      10*bytesx.KiB + 512,
			PieceLength: bytesx.KiB,
			Pieces:      make([]byte, 20*11),
		}

		// the last piece is chunks 40 and 41, credited when the torrent was resumed.
		c := newChunks(256, info)
		c.credited.Add(10)
		c.unverified.AddMany([]uint32{40, 41})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 512, s.Downloaded)
		require.EqualValues(t, 512, s.DownloadedOptimistic)
	})

	t.Run("a torrent with a zero piece length does not panic", func(t *testing.T) {
		c := newChunks(256, &metainfo.Info{})

		require.NotPanics(t, func() {
			s := c.Read(copSnapshot(&Stats{}))
			require.EqualValues(t, 0, s.Downloaded)
			require.EqualValues(t, 0, s.DownloadedOptimistic)
			require.EqualValues(t, 0, s.Remaining)
		})
	})

	t.Run("a shorter last piece that is not complete does not adjust the downloaded bytes", func(t *testing.T) {
		info := &metainfo.Info{
			Length:      10*bytesx.KiB + 512,
			PieceLength: bytesx.KiB,
			Pieces:      make([]byte, 20*11),
		}

		c := newChunks(256, info)
		c.completed.AddMany([]uint32{0, 1})

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 2*bytesx.KiB, s.Downloaded)
		require.EqualValues(t, 8*bytesx.KiB+512, s.Remaining)
	})

	t.Run("a torrent without info has nothing downloaded or remaining", func(t *testing.T) {
		c := newChunks(256, metainfo.NewInfo())

		s := c.Read(copSnapshot(&Stats{}))
		require.EqualValues(t, 0, s.Downloaded)
		require.EqualValues(t, 0, s.Remaining)
		require.Equal(t, 0, s.Completed)
	})

	t.Run("populates and returns the given stats without touching its other fields", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.completed.Add(0)

		in := &Stats{ActivePeers: 7, Seeding: true, Missing: 99}
		out := c.Read(copSnapshot(in))
		require.Same(t, in, out)
		require.Equal(t, 7, in.ActivePeers)
		require.True(t, in.Seeding)
		require.Equal(t, 0, in.Missing, "chunk counts are overwritten")
		require.Equal(t, 1, in.Completed)
	})

	t.Run("does not modify the chunks", func(t *testing.T) {
		c := newChunks(256, tinyTorrentInfo())
		c.completed.AddMany([]uint32{0, 2})
		c.unverified.AddMany([]uint32{5, 6})
		c.missing.AddMany([]uint32{20, 21})
		c.inflight.Add(22)
		c.failed.Add(23)

		completed, unverified, missing, inflight, failed := c.completed.Clone(), c.unverified.Clone(), c.missing.Clone(), c.inflight.Clone(), c.failed.Clone()

		c.Read(copSnapshot(&Stats{}))

		require.True(t, completed.Equals(c.completed))
		require.True(t, unverified.Equals(c.unverified))
		require.True(t, missing.Equals(c.missing))
		require.True(t, inflight.Equals(c.inflight))
		require.True(t, failed.Equals(c.failed))
	})
}
