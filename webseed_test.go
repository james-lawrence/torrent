package torrent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/stretchr/testify/require"
)

// sliceReadSeeker adapts an in-memory byte slice to io.ReadSeeker so
// http.ServeContent can serve BEP19 range requests from it.
type sliceReadSeeker struct {
	data []byte
	pos  int64
}

func (s *sliceReadSeeker) Read(p []byte) (int, error) {
	n := copy(p, s.data[s.pos:])
	s.pos += int64(n)
	if n == 0 {
		return 0, os.ErrClosed
	}
	return n, nil
}

func (s *sliceReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case os.SEEK_SET:
		s.pos = offset
	case os.SEEK_CUR:
		s.pos += offset
	case os.SEEK_END:
		s.pos = int64(len(s.data)) + offset
	}
	return s.pos, nil
}

func TestWebseedOnlyDownload(t *testing.T) {
	seeddir := t.TempDir()
	leechdir := t.TempDir()

	info, _, err := torrenttest.Random(seeddir, 1<<20)
	require.NoError(t, err)

	seedmd, err := NewFromInfo(info)
	require.NoError(t, err)
	infohash := seedmd.ID.String()

	content, err := os.ReadFile(filepath.Join(seeddir, infohash))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, info.Name, time.Time{}, &sliceReadSeeker{data: content})
	}))
	defer srv.Close()

	cl, err := NewClient(TestingConfig(t, leechdir))
	require.NoError(t, err)
	defer cl.Close()

	md, err := NewFromInfo(info, OptionStorage(storage.NewFile(leechdir)), OptionWebseeds([]string{srv.URL}))
	require.NoError(t, err)

	dl, added, err := cl.Start(md)
	require.NoError(t, err)
	require.True(t, added)

	npieces := int(info.NumPieces())
	require.Eventually(t, func() bool {
		return dl.Stats().Completed == npieces
	}, 15*time.Second, 20*time.Millisecond)

	got, err := os.ReadFile(filepath.Join(leechdir, infohash))
	require.NoError(t, err)
	require.Equal(t, content, got)

	wstats := dl.(*torrent).WebseedStats()
	require.Len(t, wstats, 1)
	require.Equal(t, srv.URL, wstats[0].Root)
	require.Equal(t, int64(len(content)), wstats[0].BytesFetched)
	require.Zero(t, wstats[0].Errors)
}
