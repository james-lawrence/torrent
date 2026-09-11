package bep0019_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/bep0019"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/stretchr/testify/require"
)

func TestURLSingleFile(t *testing.T) {
	info := &metainfo.Info{Name: "movie.mkv", Length: 1024}
	got, err := bep0019.URL("http://example.com/movie.mkv", info, info.UpvertedFiles()[0])
	require.NoError(t, err)
	require.Equal(t, "http://example.com/movie.mkv", got)
}

func TestURLMultiFile(t *testing.T) {
	info := &metainfo.Info{
		Name: "release",
		Files: []metainfo.FileInfo{
			{Length: 512, Path: []string{"sub dir", "file one.txt"}},
		},
	}
	got, err := bep0019.URL("http://example.com/mirror/", info, info.Files[0])
	require.NoError(t, err)
	require.Equal(t, "http://example.com/mirror/release/sub%20dir/file%20one.txt", got)
}

func TestClientFetch(t *testing.T) {
	const body = "0123456789abcdef"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		require.Equal(t, "bytes=4-7", rng)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 4-7/%d", len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body[4:8])
	}))
	defer srv.Close()

	cl := bep0019.NewClient(&net.Dialer{}, 5*time.Second)
	got, err := cl.Fetch(context.Background(), srv.URL, 4, 4)
	require.NoError(t, err)
	require.Equal(t, []byte("4567"), got)
}

func TestClientFetchIgnoresRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "whatever")
	}))
	defer srv.Close()

	cl := bep0019.NewClient(&net.Dialer{}, 5*time.Second)
	_, err := cl.Fetch(context.Background(), srv.URL, 0, 4)
	require.Error(t, err)
}
