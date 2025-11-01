package storage

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/md5x"
	"github.com/james-lawrence/torrent/metainfo"
)

func TestShortFile(t *testing.T) {
	td := t.TempDir()
	s := NewFile(td)
	info := &metainfo.Info{
		Name:        "a",
		Length:      2,
		PieceLength: bytesx.MiB,
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(InfoHashPathMaker(td, int160.Zero(), info, nil)), 0700))
	f, err := os.Create(InfoHashPathMaker(td, int160.Zero(), info, nil))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1))
	f.Close()

	ts, err := s.OpenTorrent(info, int160.Zero())
	require.NoError(t, err)

	var buf bytes.Buffer
	p := info.Piece(0)

	n, err := io.Copy(&buf, io.NewSectionReader(ts, p.Offset(), p.Length()))
	require.Equal(t, io.ErrUnexpectedEOF, err)
	require.Equal(t, int64(1), n)
}

func TestReadAllData(t *testing.T) {
	var (
		result = md5.New()
	)

	td := t.TempDir()
	s := NewFile(td)

	info, expected, err := RandomDataTorrent(td, bytesx.MiB)
	require.NoError(t, err)

	encoded, err := metainfo.Encode(info)
	require.NoError(t, err)

	ts, err := s.OpenTorrent(info, int160.FromHashedBytes(encoded))
	require.NoError(t, err)

	_, err = io.Copy(result, io.NewSectionReader(ts, 0, info.Length))
	require.NoError(t, err)
	require.Equal(t, md5x.FormatHex(expected), md5x.FormatHex(result))
}

func RandomDataTorrent(dir string, n int64, options ...metainfo.Option) (info *metainfo.Info, digested hash.Hash, err error) {
	digested = md5.New()

	src, err := IOTorrent(dir, io.TeeReader(rand.Reader, digested), n)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()

	info, err = metainfo.NewFromPath(src.Name(), options...)

	encoded, err := metainfo.Encode(info)
	if err != nil {
		return nil, nil, err
	}

	id := metainfo.NewHashFromBytes(encoded)

	dstdir := filepath.Join(dir, id.String())
	if err = os.MkdirAll(filepath.Dir(dstdir), 0700); err != nil {
		return nil, nil, err
	}

	if err = os.Rename(src.Name(), dstdir); err != nil {
		return nil, nil, err
	}

	return info, digested, nil
}

// RandomDataTorrent generates a torrent from the provided io.Reader
func IOTorrent(dir string, src io.Reader, n int64) (d *os.File, err error) {
	if d, err = os.CreateTemp(dir, "random.torrent.*.bin"); err != nil {
		return d, err
	}
	defer func() {
		if err != nil {
			os.Remove(d.Name())
		}
	}()

	if _, err = io.CopyN(d, src, n); err != nil {
		return d, err
	}

	if _, err = d.Seek(0, io.SeekStart); err != nil {
		return d, err
	}

	return d, nil
}

func BenchmarkFileStorage(b *testing.B) {
	const numFiles = 1 << 20
	const fileLength = 1

	info := &metainfo.Info{
		Name:        "test_torrent",
		PieceLength: 1024,
		Files:       make([]metainfo.FileInfo, numFiles),
	}
	totalLength := int64(0)
	for i := 0; i < numFiles; i++ {
		info.Files[i] = metainfo.FileInfo{
			Length: fileLength,
			Path:   []string{fmt.Sprintf("file%d", i)},
		}
		totalLength += fileLength
	}
	info.Length = totalLength

	dir := b.TempDir()
	fs := NewFile(dir)

	ti, err := fs.OpenTorrent(info, int160.Zero())
	if err != nil {
		b.Fatal(err)
	}
	defer ti.Close()

	data := []byte{0xFF}

	_, err = ti.WriteAt(data, totalLength-1)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.Run("WriteAtLastByte", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := ti.WriteAt(data, totalLength-1)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
