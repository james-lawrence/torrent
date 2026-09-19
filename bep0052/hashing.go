package bep0052

import (
	"crypto/sha256"
	"errors"
	"io"
)

// ErrPieceLengthNotPow2 is returned when a piece length is not a power of
// two multiple of BlockSize, as required by BEP 52.
var ErrPieceLengthNotPow2 = errors.New("bep0052: piece length must be a power-of-two multiple of BlockSize")

// blocksPerPiece returns the number of BlockSize blocks in a full piece,
// and the number of tree layers between a block leaf and a piece hash.
func blocksPerPiece(pieceLength int64) (blocks int, layer int, err error) {
	if pieceLength <= 0 || pieceLength%BlockSize != 0 {
		return 0, 0, ErrPieceLengthNotPow2
	}

	blocks = int(pieceLength / BlockSize)
	if nextPow2(blocks) != blocks {
		return 0, 0, ErrPieceLengthNotPow2
	}

	for n := blocks; n > 1; n >>= 1 {
		layer++
	}

	return blocks, layer, nil
}

// HashBlocks splits r into BlockSize chunks and returns the SHA-256 hash
// of each. The final block may be shorter than BlockSize; it is hashed as-is
// (not zero-padded), per BEP 52.
func HashBlocks(r io.Reader) ([]Hash, error) {
	var hashes []Hash

	buf := make([]byte, BlockSize)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			hashes = append(hashes, Hash(sha256.Sum256(buf[:n])))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	return hashes, nil
}

// PieceHashes groups block hashes into per-piece hashes (the "piece
// layer" for a file, per BEP 52), given a piece length in bytes.
func PieceHashes(blocks []Hash, pieceLength int64) ([]Hash, error) {
	perPiece, _, err := blocksPerPiece(pieceLength)
	if err != nil {
		return nil, err
	}

	var pieces []Hash
	for i := 0; i < len(blocks); i += perPiece {
		end := i + perPiece
		if end > len(blocks) {
			end = len(blocks)
		}
		pieces = append(pieces, merkleRootAtLayer(blocks[i:end], 0))
	}

	return pieces, nil
}

// PiecesRoot computes a file's "pieces root" (the top-level merkle root
// stored per-file in a BEP 52 file tree entry) from its per-piece hashes.
func PiecesRoot(pieces []Hash, pieceLength int64) (Hash, error) {
	_, layer, err := blocksPerPiece(pieceLength)
	if err != nil {
		return Hash{}, err
	}

	return merkleRootAtLayer(pieces, layer), nil
}

// FileHashes reads all of r, computing its block hashes, per-piece hashes,
// and overall pieces root for the given piece length, in one pass.
func FileHashes(r io.Reader, pieceLength int64) (blocks, pieces []Hash, root Hash, err error) {
	blocks, err = HashBlocks(r)
	if err != nil {
		return nil, nil, Hash{}, err
	}

	pieces, err = PieceHashes(blocks, pieceLength)
	if err != nil {
		return nil, nil, Hash{}, err
	}

	root, err = PiecesRoot(pieces, pieceLength)
	if err != nil {
		return nil, nil, Hash{}, err
	}

	return blocks, pieces, root, nil
}
