package storage

import (
	"errors"
	"io"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/metainfo"
)

// Represents data storage for an unspecified torrent.
type ClientImpl interface {
	OpenTorrent(info *metainfo.Info, infoHash int160.T) (TorrentImpl, error)
	Close() error
}

// Data storage bound to a torrent.
type TorrentImpl interface {
	io.ReaderAt
	io.WriterAt
	Close() error
}

func ErrClosed() error {
	return errors.New("storage closed")
}
