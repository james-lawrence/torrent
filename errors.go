package torrent

import (
	"github.com/james-lawrence/torrent/internal/errorsx"
)

func ErrTorrentClosed() error {
	return errorsx.New("torrent closed")
}

const (
	ErrTorrentNotActive                     = errorsx.String("torrent not active")
	ErrTorrentAttemptedToPersistNilMetadata = errorsx.String("cannot persist torrent metadata when missing info")
)
