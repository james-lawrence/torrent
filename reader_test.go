package torrent_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/james-lawrence/torrent/torrenttestx"
	"github.com/stretchr/testify/require"
)

func TestReader(t *testing.T) {
	t.Run("reads full content of a single file torrent", func(t *testing.T) {
		ctx, done := testx.Context(t)
		defer done()

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, dir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer cl.Close()

		md, err := torrent.NewFromMetaInfo(mi)
		require.NoError(t, err)

		tt, _, err := cl.Start(md)
		require.NoError(t, err)
		defer cl.Stop(md)

		require.NoError(t, torrent.Verify(ctx, tt))

		r := torrent.NewReader(tt)
		defer r.Close()

		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, testutil.GreetingFileContents, string(got))
	})

	t.Run("reads correct content per file in a multi-file torrent", func(t *testing.T) {
		ctx, done := testx.Context(t)
		defer done()

		dir := t.TempDir()
		paths := []string{"a.bin", "sub/b.bin", "sub/deep/c.bin"}
		info, err := torrenttest.Tree(dir, cryptox.NewChaCha8(t.Name()), int64(bytesx.KiB), 2*int64(bytesx.KiB), paths)
		require.NoError(t, err)

		encoded, err := metainfo.Encode(info)
		require.NoError(t, err)
		id := metainfo.NewHashFromBytes(encoded)
		root := filepath.Join(dir, id.String())

		cl, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, dir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer cl.Close()

		md, err := torrent.NewFromInfo(info)
		require.NoError(t, err)

		tt, _, err := cl.Start(md)
		require.NoError(t, err)
		defer cl.Stop(md)

		require.NoError(t, torrent.Verify(ctx, tt))

		for _, f := range torrent.FilesOf(tt) {
			expected, err := os.ReadFile(filepath.Join(append([]string{root}, f.FileInfo().Path...)...))
			require.NoError(t, err)

			r := f.NewReader()
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())

			require.Equal(t, expected, got, "file %q content mismatch", f.Path())
		}
	})

	t.Run("seek", func(t *testing.T) {
		ctx, done := testx.Context(t)
		defer done()

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, dir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer cl.Close()

		md, err := torrent.NewFromMetaInfo(mi)
		require.NoError(t, err)

		tt, _, err := cl.Start(md)
		require.NoError(t, err)
		defer cl.Stop(md)

		require.NoError(t, torrent.Verify(ctx, tt))

		r := torrent.NewReader(tt)
		defer r.Close()

		t.Run("SeekStart reads from absolute offset", func(t *testing.T) {
			pos, err := r.Seek(7, io.SeekStart)
			require.NoError(t, err)
			require.EqualValues(t, 7, pos)

			b := make([]byte, 5)
			_, err = io.ReadFull(r, b)
			require.NoError(t, err)
			require.Equal(t, "world", string(b))
		})

		t.Run("SeekCurrent advances relative to position", func(t *testing.T) {
			_, err := r.Seek(0, io.SeekStart)
			require.NoError(t, err)
			_, err = r.Seek(7, io.SeekCurrent)
			require.NoError(t, err)

			b := make([]byte, 5)
			_, err = io.ReadFull(r, b)
			require.NoError(t, err)
			require.Equal(t, "world", string(b))
		})

		t.Run("SeekEnd reads from end of stream", func(t *testing.T) {
			pos, err := r.Seek(1, io.SeekEnd)
			require.NoError(t, err)
			require.EqualValues(t, int64(len(testutil.GreetingFileContents))-1, pos)

			b := make([]byte, 1)
			_, err = io.ReadFull(r, b)
			require.NoError(t, err)
			require.Equal(t, "\n", string(b))
		})

		t.Run("unsupported whence returns an error", func(t *testing.T) {
			_, err := r.Seek(0, 99)
			require.True(t, errors.Is(err, errors.ErrUnsupported))
		})
	})

	t.Run("read past end of torrent returns io.EOF", func(t *testing.T) {
		ctx, done := testx.Context(t)
		defer done()

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, dir, torrent.ClientConfigSeed(true))))
		require.NoError(t, err)
		defer cl.Close()

		md, err := torrent.NewFromMetaInfo(mi)
		require.NoError(t, err)

		tt, _, err := cl.Start(md)
		require.NoError(t, err)
		defer cl.Stop(md)

		require.NoError(t, torrent.Verify(ctx, tt))

		r := torrent.NewReader(tt)
		defer r.Close()

		_, err = r.Seek(0, io.SeekEnd)
		require.NoError(t, err)

		n, err := r.Read(make([]byte, 1))
		require.Zero(t, n)
		require.ErrorIs(t, err, io.EOF)
	})

	t.Run("read blocks until data is available and unblocks on Close", func(t *testing.T) {
		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		// leecher never gets a peer, so it never receives data.
		cl, err := torrenttestx.Autosocket(t).Bind(torrent.NewClient(torrent.TestingConfig(t, t.TempDir(), torrent.ClientConfigSeed(false))))
		require.NoError(t, err)
		defer cl.Close()

		md, err := torrent.NewFromMetaInfo(mi, torrent.OptionChunk(2))
		require.NoError(t, err)

		tt, _, err := cl.Start(md)
		require.NoError(t, err)
		defer cl.Stop(md)

		r := torrent.NewReader(tt)

		done := make(chan error, 1)
		go func() {
			_, err := r.Read(make([]byte, 1))
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("expected read to block, but it returned: %v", err)
		case <-time.After(100 * time.Millisecond):
		}

		require.NoError(t, r.Close())

		select {
		case err := <-done:
			require.ErrorIs(t, err, io.ErrClosedPipe)
		case <-time.After(5 * time.Second):
			t.Fatal("expected blocked read to unblock after Close")
		}
	})
}
