// Package bep0052 implements the data structures and algorithms defined in
// BitTorrent Enhancement Proposal 52 (BEP 52): The BitTorrent Protocol
// Specification v2. It is a standalone building block: it has no
// dependency on and is not wired into metainfo, storage, the wire
// protocol, tracker, or dht. A future integration effort is expected to
// consume this package directly.
package bep0052

import "crypto/sha256"

// BlockSize is the fixed leaf block size used for BEP 52 merkle trees,
// independent of a torrent's piece length.
const BlockSize = 16 * 1024

// HashSize is the size of a BEP 52 (SHA-256) hash.
const HashSize = sha256.Size

// Hash is a single SHA-256 hash node.
type Hash [HashSize]byte

var zeroBlock [BlockSize]byte

// padHash returns the fixed hash used to pad an incomplete merkle layer.
// layer 0 is the hash of an all-zero leaf block; layer n is the hash of
// two concatenated copies of the layer n-1 pad hash. This mirrors the
// "hash of a tree of all zero blocks" padding described in BEP 52.
func padHash(layer int) Hash {
	h := Hash(sha256.Sum256(zeroBlock[:]))
	for i := 0; i < layer; i++ {
		h = combine(h, h)
	}
	return h
}

func combine(left, right Hash) Hash {
	buf := make([]byte, 0, 2*HashSize)
	buf = append(buf, left[:]...)
	buf = append(buf, right[:]...)
	return Hash(sha256.Sum256(buf))
}

func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// merkleRootAtLayer computes the root of a merkle tree built from leaves,
// where leaves live at the given layer height above the true leaf (block)
// layer (layer 0 == the leaves are block hashes themselves). Missing
// leaves needed to pad the layer to a power of two are filled using the
// appropriate pad hash for that layer, per BEP 52.
func merkleRootAtLayer(leaves []Hash, layer int) Hash {
	n := nextPow2(len(leaves))

	level := make([]Hash, n)
	copy(level, leaves)
	for i := len(leaves); i < n; i++ {
		level[i] = padHash(layer)
	}

	for len(level) > 1 {
		next := make([]Hash, len(level)/2)
		for i := range next {
			next[i] = combine(level[2*i], level[2*i+1])
		}
		level = next
		layer++
	}

	return level[0]
}

// BuildTree computes the merkle root of leaf (block) hashes.
func BuildTree(leaves []Hash) Hash {
	return merkleRootAtLayer(leaves, 0)
}

// VerifyProof recomputes a merkle root from a leaf hash, its index within
// the tree, and a bottom-up list of sibling hashes, and reports whether it
// matches root.
func VerifyProof(leaf Hash, index int, siblings []Hash, root Hash) bool {
	h := leaf
	idx := index
	for _, sib := range siblings {
		if idx%2 == 0 {
			h = combine(h, sib)
		} else {
			h = combine(sib, h)
		}
		idx /= 2
	}
	return h == root
}
