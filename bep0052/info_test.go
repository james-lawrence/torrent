package bep0052_test

import (
	"crypto/sha256"
	"testing"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/bep0052"
	"github.com/stretchr/testify/require"
)

func TestFileTreeRoundTrip(t *testing.T) {
	t.Run("single file at top level", func(t *testing.T) {
		root := bep0052.Hash(sha256.Sum256([]byte("root")))
		tree := bep0052.FileTree{
			"video.mp4": bep0052.FileTreeNode{
				File: &bep0052.FileTreeEntry{Length: 12345, PiecesRoot: root},
			},
		}

		encoded, err := bencode.Marshal(tree)
		require.NoError(t, err)

		var decoded bep0052.FileTree
		require.NoError(t, bencode.Unmarshal(encoded, &decoded))

		require.Len(t, decoded, 1)
		node := decoded["video.mp4"]
		require.NotNil(t, node.File)
		require.Equal(t, int64(12345), node.File.Length)
		require.Equal(t, root, node.File.PiecesRoot)
	})

	t.Run("nested directory structure", func(t *testing.T) {
		root := bep0052.Hash(sha256.Sum256([]byte("nested-root")))
		tree := bep0052.FileTree{
			"subdir": bep0052.FileTreeNode{
				Dir: bep0052.FileTree{
					"file.txt": bep0052.FileTreeNode{
						File: &bep0052.FileTreeEntry{Length: 42, PiecesRoot: root},
					},
				},
			},
		}

		encoded, err := bencode.Marshal(tree)
		require.NoError(t, err)

		var decoded bep0052.FileTree
		require.NoError(t, bencode.Unmarshal(encoded, &decoded))

		subdir := decoded["subdir"]
		require.Nil(t, subdir.File)
		require.NotNil(t, subdir.Dir)

		file := subdir.Dir["file.txt"]
		require.NotNil(t, file.File)
		require.Equal(t, int64(42), file.File.Length)
		require.Equal(t, root, file.File.PiecesRoot)
	})
}

func TestPieceLayersRoundTrip(t *testing.T) {
	root := bep0052.Hash(sha256.Sum256([]byte("pieces-root")))
	h1 := bep0052.Hash(sha256.Sum256([]byte("piece-1")))
	h2 := bep0052.Hash(sha256.Sum256([]byte("piece-2")))

	layers := make(bep0052.PieceLayers)
	layers.Set(root, []bep0052.Hash{h1, h2})

	encoded, err := bencode.Marshal(layers)
	require.NoError(t, err)

	var decoded bep0052.PieceLayers
	require.NoError(t, bencode.Unmarshal(encoded, &decoded))

	require.Equal(t, []bep0052.Hash{h1, h2}, decoded.Get(root))
}
