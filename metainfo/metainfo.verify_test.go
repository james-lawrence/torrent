package metainfo

import (
	"bytes"
	"io"
	"testing"

	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/stretchr/testify/require"
)

func TestMetaInfoVerify(t *testing.T) {
	t.Run("valid data passes verification", func(t *testing.T) {
		data := make([]byte, 128*bytesx.KiB)
		_, err := io.ReadFull(cryptox.NewChaCha8(t.Name()), data)
		require.NoError(t, err)

		info, err := NewFromReader(bytes.NewReader(data), OptionPieceLength(bytesx.KiB))
		require.NoError(t, err)

		infobytes, err := Encode(*info)
		require.NoError(t, err)

		mi := MetaInfo{InfoBytes: infobytes}
		require.NoError(t, mi.Verify(bytes.NewReader(data)))
	})

	t.Run("corrupted data fails verification", func(t *testing.T) {
		data := make([]byte, 128*bytesx.KiB)
		_, err := io.ReadFull(cryptox.NewChaCha8(t.Name()), data)
		require.NoError(t, err)

		info, err := NewFromReader(bytes.NewReader(data), OptionPieceLength(bytesx.KiB))
		require.NoError(t, err)

		infobytes, err := Encode(*info)
		require.NoError(t, err)

		corrupted := make([]byte, len(data))
		_, err = io.ReadFull(cryptox.NewChaCha8("different-seed"), corrupted)
		require.NoError(t, err)

		mi := MetaInfo{InfoBytes: infobytes}
		require.Error(t, mi.Verify(bytes.NewReader(corrupted)))
	})
}
