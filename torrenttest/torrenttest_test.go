package torrenttest

import (
	"io"
	"testing"

	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/stretchr/testify/require"
)

func TestRandom(t *testing.T) {
	t.Run("produces valid single-file info", func(t *testing.T) {
		dir := t.TempDir()
		info, digest, err := Random(dir, 1024)
		require.NoError(t, err)
		require.NotNil(t, info)
		require.NotNil(t, digest)
		require.Greater(t, info.TotalLength(), int64(0))
		require.Empty(t, info.Files, "single-file torrent should have no Files slice")
	})
}

func TestSeeded(t *testing.T) {
	t.Run("same data produces same content digest", func(t *testing.T) {
		const length = 4 * bytesx.KiB
		info1, digest1, err := Seeded(t.TempDir(), length, io.LimitReader(cryptox.NewChaCha8(t.Name()), length))
		require.NoError(t, err)
		require.NotNil(t, info1)
		require.NotNil(t, digest1)

		_, digest2, err := Seeded(t.TempDir(), length, io.LimitReader(cryptox.NewChaCha8(t.Name()), length))
		require.NoError(t, err)

		require.Equal(t, digest1.Sum(nil), digest2.Sum(nil))
	})
}

func TestRandomTree(t *testing.T) {
	t.Run("no dirs produces flat file structure", func(t *testing.T) {
		info, err := RandomTree(t.TempDir(), cryptox.NewChaCha8(t.Name()), 3, 512, 1024, 0)
		require.NoError(t, err)
		require.Len(t, info.Files, 3)

		for _, f := range info.Files {
			require.Len(t, f.Path, 1, "expected flat file, got path %v", f.Path)
		}
	})

	t.Run("with dirs produces nested files", func(t *testing.T) {
		info, err := RandomTree(t.TempDir(), cryptox.NewChaCha8(t.Name()), 5, 512, 1024, 3)
		require.NoError(t, err)
		require.Len(t, info.Files, 5)

		nested := 0
		for _, f := range info.Files {
			if len(f.Path) > 1 {
				nested++
			}
		}
		require.Greater(t, nested, 0, "expected at least one nested file")
	})

	t.Run("produces valid torrent", func(t *testing.T) {
		info, err := RandomTree(t.TempDir(), cryptox.NewChaCha8(t.Name()), 4, 256, 512, 2)
		require.NoError(t, err)
		require.NotNil(t, info)

		encoded, err := metainfo.Encode(info)
		require.NoError(t, err)
		require.NotEmpty(t, encoded)

		id := metainfo.NewHashFromBytes(encoded)
		require.NotZero(t, id)
	})

	t.Run("dirs=0 matches RandomMulti flat structure", func(t *testing.T) {
		info1, err := RandomMulti(t.TempDir(), 4, 256, 512)
		require.NoError(t, err)
		require.Len(t, info1.Files, 4)

		info2, err := RandomTree(t.TempDir(), cryptox.NewChaCha8(t.Name()), 4, 256, 512, 0)
		require.NoError(t, err)
		require.Len(t, info2.Files, 4)

		for _, f := range info1.Files {
			require.Len(t, f.Path, 1)
		}
		for _, f := range info2.Files {
			require.Len(t, f.Path, 1)
		}
	})
}
