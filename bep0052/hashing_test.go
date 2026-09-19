package bep0052_test

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/james-lawrence/torrent/bep0052"
	"github.com/stretchr/testify/require"
)

func TestHashBlocks(t *testing.T) {
	t.Run("splits input into full and short final block", func(t *testing.T) {
		full := bytes.Repeat([]byte{0x11}, bep0052.BlockSize)
		short := []byte("trailing partial block")

		data := append(append([]byte{}, full...), short...)
		hashes, err := bep0052.HashBlocks(bytes.NewReader(data))
		require.NoError(t, err)
		require.Len(t, hashes, 2)
		require.Equal(t, bep0052.Hash(sha256.Sum256(full)), hashes[0])
		require.Equal(t, bep0052.Hash(sha256.Sum256(short)), hashes[1])
	})

	t.Run("empty input yields no blocks", func(t *testing.T) {
		hashes, err := bep0052.HashBlocks(bytes.NewReader(nil))
		require.NoError(t, err)
		require.Empty(t, hashes)
	})
}

func TestPieceHashes(t *testing.T) {
	t.Run("rejects piece length that is not a power-of-two multiple of block size", func(t *testing.T) {
		blocks := []bep0052.Hash{{}, {}}
		_, err := bep0052.PieceHashes(blocks, 3*bep0052.BlockSize)
		require.ErrorIs(t, err, bep0052.ErrPieceLengthNotPow2)
	})

	t.Run("single block per piece yields one hash per block", func(t *testing.T) {
		a := bep0052.Hash(sha256.Sum256([]byte("a")))
		b := bep0052.Hash(sha256.Sum256([]byte("b")))

		pieces, err := bep0052.PieceHashes([]bep0052.Hash{a, b}, bep0052.BlockSize)
		require.NoError(t, err)
		require.Equal(t, []bep0052.Hash{a, b}, pieces)
	})

	t.Run("multiple blocks per piece combine into a piece merkle root", func(t *testing.T) {
		a := bep0052.Hash(sha256.Sum256([]byte("a")))
		b := bep0052.Hash(sha256.Sum256([]byte("b")))
		c := bep0052.Hash(sha256.Sum256([]byte("c")))

		pieces, err := bep0052.PieceHashes([]bep0052.Hash{a, b, c}, 2*bep0052.BlockSize)
		require.NoError(t, err)
		require.Len(t, pieces, 2)
		require.Equal(t, bep0052.BuildTree([]bep0052.Hash{a, b}), pieces[0])
		require.Equal(t, bep0052.BuildTree([]bep0052.Hash{c}), pieces[1])
	})
}

func TestFileHashes(t *testing.T) {
	t.Run("small single-piece file's pieces root equals its piece hash", func(t *testing.T) {
		content := []byte("hello world")
		_, pieces, root, err := bep0052.FileHashes(bytes.NewReader(content), bep0052.BlockSize)
		require.NoError(t, err)
		require.Len(t, pieces, 1)
		require.Equal(t, pieces[0], root)
	})

	t.Run("multi-piece file's pieces root is deterministic and reproducible", func(t *testing.T) {
		content := bytes.Repeat([]byte{0x42}, 5*bep0052.BlockSize+123)

		_, pieces1, root1, err := bep0052.FileHashes(bytes.NewReader(content), 2*bep0052.BlockSize)
		require.NoError(t, err)

		_, pieces2, root2, err := bep0052.FileHashes(bytes.NewReader(content), 2*bep0052.BlockSize)
		require.NoError(t, err)

		require.Equal(t, pieces1, pieces2)
		require.Equal(t, root1, root2)
	})
}
