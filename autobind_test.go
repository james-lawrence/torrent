package torrent_test

import (
	"testing"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/utpx"
	"github.com/james-lawrence/torrent/internal/x/bytesx"
	"github.com/stretchr/testify/require"
)

func TestSocketsBindSockets(t *testing.T) {
	autosocket := func() torrent.ProxySocket {
		s, err := utpx.New("udp", "localhost:0")
		require.NoError(t, err)
		return torrent.NewProxySocket(s, nil)
	}

	s, err := torrent.NewSocketsBind(autosocket()).Bind(torrent.NewClient(TestingSeedConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer s.Close()

	l, err := torrent.NewSocketsBind(autosocket()).Bind(torrent.NewClient(TestingLeechConfig(t, t.TempDir())))
	require.NoError(t, err)
	defer l.Close()
	testTransferRandomData(t, bytesx.KiB, s, l)
}
