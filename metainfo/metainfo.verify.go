package metainfo

import (
	"bytes"
	"errors"
	"io"
)

// Verify checks that the data in src matches the piece hashes in mi.
// Returns nil if the data is valid, or an error describing the mismatch.
func (mi MetaInfo) Verify(src io.ReaderAt) error {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return err
	}

	computed, err := NewFromReader(
		io.NewSectionReader(src, 0, info.TotalLength()),
		OptionPieceLength(info.PieceLength),
	)
	if err != nil {
		return err
	}

	if !bytes.Equal(computed.Pieces, info.Pieces) {
		return errors.New("piece hash mismatch: data does not match torrent")
	}

	return nil
}
