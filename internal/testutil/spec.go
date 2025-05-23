package testutil

import (
	"io"
	"strings"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/metainfo"
)

// File ...
type File struct {
	Name string
	Data string
}

// Torrent ...
type Torrent struct {
	Files []File
	Name  string
}

func (t *Torrent) IsDir() bool {
	return len(t.Files) == 1 && t.Files[0].Name == ""
}

func (t *Torrent) GetFile(name string) *File {
	if t.IsDir() && t.Name == name {
		return &t.Files[0]
	}
	for _, f := range t.Files {
		if f.Name == name {
			return &f
		}
	}
	return nil
}

func (t *Torrent) Info(pieceLength int64) metainfo.Info {
	info := metainfo.Info{
		Name:        t.Name,
		PieceLength: pieceLength,
	}
	if t.IsDir() {
		info.Length = int64(len(t.Files[0].Data))
	}
	err := info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(t.GetFile(strings.Join(fi.Path, "/")).Data)), nil
	})
	errorsx.Panic(err)
	return info
}

func (t *Torrent) Metainfo(pieceLength int64) *metainfo.MetaInfo {
	mi := metainfo.MetaInfo{}
	var err error
	mi.InfoBytes, err = bencode.Marshal(t.Info(pieceLength))
	errorsx.Panic(err)
	return &mi
}
