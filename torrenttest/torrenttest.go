// Package torrenttest contains functions for testing torrent-related behaviour.
//

package torrenttest

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"hash"
	"io"
	"log"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/stretchr/testify/require"
)

// Seeded creates a single-file torrent of n bytes read from r, writing it under dir.
// The file is renamed to its info hash. Returns the metainfo and an MD5 digest of the content,
// allowing callers to verify the same reader produces the same digest across calls.
func Seeded(dir string, n uint64, r io.Reader, options ...metainfo.Option) (info *metainfo.Info, digested hash.Hash, err error) {
	digested = md5.New()

	src, err := IOTorrent(dir, io.TeeReader(r, digested), n)
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

// Random creates a single-file torrent of n bytes from crypto/rand.
func Random(dir string, n uint64, options ...metainfo.Option) (info *metainfo.Info, digested hash.Hash, err error) {
	return Seeded(dir, n, rand.Reader, options...)
}

// RandomMulti creates a flat multi-file torrent with n files under dir, each between min and max bytes.
// Equivalent to RandomTree with dirs=0.
func RandomMulti(dir string, n int, min int64, max int64, options ...metainfo.Option) (info *metainfo.Info, err error) {
	return RandomTree(dir, rand.Reader, n, min, max, 0, options...)
}

func adddir(parent string, depth int64) string {
	if depth <= 0 {
		return parent
	}

	subdir, err := os.MkdirTemp(parent, "*")
	if err != nil {
		log.Println("failed to make tmp directory", err, "returning", parent)
		return parent
	}

	return adddir(subdir, depth-1)
}

// RandomTree creates a multi-file torrent with n files under dir, each between min and max bytes.
// dirs controls how many subdirectory chains (each 4 levels deep) are created; files are placed
// randomly across them. When dirs is 0 the result is a flat directory, matching RandomMulti.
func RandomTree(dir string, r io.Reader, n int, min int64, max int64, dirs int64, options ...metainfo.Option) (info *metainfo.Info, err error) {
	root, err := os.MkdirTemp(dir, "multi.torrent.*")
	if err != nil {
		return nil, err
	}

	addfile := func(dir string) error {
		src, err := IOTorrent(dir, r, uint64(mrand.Int64N(max-min)+min))
		return errorsx.Compact(err, src.Close())
	}

	_dirs := []string{root}
	for range dirs {
		_dirs = append(_dirs, adddir(root, 4))
	}

	for range n {
		if err := addfile(_dirs[mrand.IntN(len(_dirs))]); err != nil {
			return nil, err
		}
	}

	info, err = metainfo.NewFromPath(root, options...)
	if err != nil {
		return nil, err
	}

	encoded, err := metainfo.Encode(info)
	if err != nil {
		return nil, err
	}

	id := metainfo.NewHashFromBytes(encoded)

	dstdir := filepath.Join(dir, id.String())
	if err = os.Rename(root, dstdir); err != nil {
		return nil, err
	}

	return info, nil
}

// Tree creates a multi-file torrent whose layout matches the explicit paths slice (e.g. "foo/bar/baz.txt").
// Intermediate directories are created as needed. Each file is filled from r with a random size between
// min and max bytes.
func Tree(dir string, r io.Reader, min int64, max int64, paths []string, options ...metainfo.Option) (info *metainfo.Info, err error) {
	root, err := os.MkdirTemp(dir, "multi.torrent.*")
	if err != nil {
		return nil, err
	}

	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			return nil, err
		}

		f, err := os.Create(full)
		if err != nil {
			return nil, err
		}

		size := mrand.Int64N(max-min) + min
		_, err = io.CopyN(f, r, size)
		if err = errorsx.Compact(err, f.Sync(), f.Close()); err != nil {
			return nil, err
		}
	}

	info, err = metainfo.NewFromPath(root, options...)
	if err != nil {
		return nil, err
	}

	encoded, err := metainfo.Encode(info)
	if err != nil {
		return nil, err
	}

	id := metainfo.NewHashFromBytes(encoded)

	dstdir := filepath.Join(dir, id.String())
	if err = os.Rename(root, dstdir); err != nil {
		return nil, err
	}

	return info, nil
}

// IOTorrent creates a temp file under dir, copies n bytes from src into it, syncs, and seeks back to
// the start. The caller is responsible for closing the returned file.
func IOTorrent(dir string, src io.Reader, n uint64) (d *os.File, err error) {
	if d, err = os.CreateTemp(dir, "random.torrent.*.bin"); err != nil {
		return d, err
	}
	defer func() {
		if err == nil {
			return
		}

		// cleanup failed creation.
		os.Remove(d.Name())
	}()

	if _, err = io.CopyN(d, src, int64(n)); err != nil {
		return d, err
	}

	// important without this we corrupt the torrent on disk during rename?
	if err = d.Sync(); err != nil {
		return d, err
	}

	if _, err = d.Seek(0, io.SeekStart); err != nil {
		return d, err
	}

	return d, nil
}

// RequireMessageType asserts that actual matches the expected MessageType, failing the test if not.
func RequireMessageType(t testing.TB, expected, actual btprotocol.MessageType) {
	require.Equal(t, expected, actual, "expected %s received %s", expected, actual)
}

// FilterMessageType returns only the messages whose Type matches mt.
func FilterMessageType(mt btprotocol.MessageType, msgs ...btprotocol.Message) []btprotocol.Message {
	return slicesx.Filter(func(m btprotocol.Message) bool {
		return m.Type == mt
	}, msgs...)
}

// ReadUntil reads messages from reader until a message of type m is received or the test context is
// cancelled. Returns all messages read, including the terminal one.
func ReadUntil(t testing.TB, m btprotocol.MessageType, reader func() (btprotocol.Message, error)) (result []btprotocol.Message, _ error) {
	ctx := t.Context()
	for {
		msg, err := reader()
		if err != nil {
			return result, err
		}

		result = append(result, msg)

		if msg.Type == m {
			return result, nil
		}

		select {
		case <-ctx.Done():
			return result, context.Cause(ctx)
		default:
		}
	}
}
