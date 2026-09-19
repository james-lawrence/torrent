package bep0052_test

import (
	"crypto/sha256"
	"testing"

	"github.com/james-lawrence/torrent/bep0052"
	"github.com/stretchr/testify/require"
)

func TestBuildTree(t *testing.T) {
	t.Run("single leaf root equals the leaf itself", func(t *testing.T) {
		leaf := bep0052.Hash(sha256.Sum256([]byte("leaf")))
		root := bep0052.BuildTree([]bep0052.Hash{leaf})
		require.Equal(t, leaf, root)
	})

	t.Run("two leaves combine deterministically", func(t *testing.T) {
		a := bep0052.Hash(sha256.Sum256([]byte("a")))
		b := bep0052.Hash(sha256.Sum256([]byte("b")))

		want := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
		root := bep0052.BuildTree([]bep0052.Hash{a, b})
		require.Equal(t, bep0052.Hash(want), root)
	})

	t.Run("odd leaf count pads with zero-block hash", func(t *testing.T) {
		a := sha256.Sum256([]byte("a"))
		b := sha256.Sum256([]byte("b"))
		c := sha256.Sum256([]byte("c"))
		zero := sha256.Sum256(make([]byte, bep0052.BlockSize))

		ab := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
		czero := sha256.Sum256(append(append([]byte{}, c[:]...), zero[:]...))
		want := sha256.Sum256(append(append([]byte{}, ab[:]...), czero[:]...))

		root := bep0052.BuildTree([]bep0052.Hash{a, b, c})
		require.Equal(t, bep0052.Hash(want), root)
	})
}

func TestVerifyProof(t *testing.T) {
	a := bep0052.Hash(sha256.Sum256([]byte("a")))
	b := bep0052.Hash(sha256.Sum256([]byte("b")))
	c := bep0052.Hash(sha256.Sum256([]byte("c")))
	d := bep0052.Hash(sha256.Sum256([]byte("d")))

	root := bep0052.BuildTree([]bep0052.Hash{a, b, c, d})

	ab := sha256.Sum256(append(append([]byte{}, a[:]...), b[:]...))
	cd := sha256.Sum256(append(append([]byte{}, c[:]...), d[:]...))
	_ = ab
	_ = cd

	t.Run("valid proof for leaf a", func(t *testing.T) {
		siblings := []bep0052.Hash{b, bep0052.Hash(cd)}
		require.True(t, bep0052.VerifyProof(a, 0, siblings, root))
	})

	t.Run("valid proof for leaf d", func(t *testing.T) {
		siblings := []bep0052.Hash{c, bep0052.Hash(ab)}
		require.True(t, bep0052.VerifyProof(d, 3, siblings, root))
	})

	t.Run("tampered leaf fails verification", func(t *testing.T) {
		siblings := []bep0052.Hash{b, bep0052.Hash(cd)}
		require.False(t, bep0052.VerifyProof(c, 0, siblings, root))
	})
}
