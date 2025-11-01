package storage

import (
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/metainfo"
)

type Client struct {
	ci ClientImpl
}

func NewClient(cl ClientImpl) *Client {
	return &Client{cl}
}

func (cl Client) OpenTorrent(info *metainfo.Info, infoHash int160.T) (TorrentImpl, error) {
	t, err := cl.ci.OpenTorrent(info, infoHash)
	return t, err
}

func NewTorrent(ti TorrentImpl) TorrentImpl {
	return ti
}
