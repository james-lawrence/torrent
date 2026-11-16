package torrent_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/torrenttestx"
)

// torrents discovered through a tracker must keep working when the dht is passive (ex. metered connections).
func TestPassiveDHTTracker(t *testing.T) {
	cases := []struct {
		name    string
		seeder  bool
		leecher bool
	}{
		{name: "passive leecher", leecher: true},
		{name: "passive seeder", seeder: true},
		{name: "passive seeder and leecher", seeder: true, leecher: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, done := testx.Context(t)
			defer done()

			seedid, leechid := int160.Random(), int160.Random()
			seeddht := torrenttestx.QuickDHT(t, dht.OptionNodeID(seedid), dht.OptionPassive(c.seeder))
			leechdht := torrenttestx.QuickDHT(t, dht.OptionNodeID(leechid), dht.OptionPassive(c.leecher))

			seeddir := t.TempDir()
			mi := testutil.GreetingTestTorrent(seeddir)

			seeder, err := autobind.NewLoopback(autobind.EnableDHT(seeddht)).Bind(
				torrent.NewClient(torrent.TestingConfig(t, seeddir, torrent.ClientConfigSeed(true))),
			)
			require.NoError(t, err)
			defer seeder.Close()

			seedmd, err := torrent.NewFromMetaInfo(mi, torrent.OptionStorage(storage.NewFile(seeddir)))
			require.NoError(t, err)

			seedtorrent, _, err := seeder.Start(seedmd)
			require.NoError(t, err)
			require.NoError(t, torrent.Verify(ctx, seedtorrent))
			_, err = torrent.DownloadInto(ctx, io.Discard, seedtorrent, torrent.TuneSeeding)
			require.NoError(t, err)

			// the tracker is the only way the leecher can discover the seeder.
			var compact []byte
			for _, addr := range torrenttestx.AddrPorts(seeder) {
				if addr.Addr().Is4() {
					compact = binary.BigEndian.AppendUint16(addr.Addr().AsSlice(), addr.Port())
					break
				}
			}
			require.Len(t, compact, 6, "seeder must listen on an ipv4 address")

			var announces atomic.Int32
			tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				announces.Add(1)
				fmt.Fprintf(w, "d8:completei1e10:incompletei1e8:intervali1800e5:peers6:%se", compact)
			}))
			defer tracker.Close()

			leechdir := t.TempDir()
			leecher, err := autobind.NewLoopback(autobind.EnableDHT(leechdht)).Bind(
				torrent.NewClient(torrent.TestingConfig(t, leechdir, torrent.ClientConfigSeed(false))),
			)
			require.NoError(t, err)
			defer leecher.Close()

			leechmd, err := torrent.New(
				mi.HashInfoBytes(),
				torrent.OptionStorage(storage.NewFile(leechdir)),
				torrent.OptionChunk(2),
				torrent.OptionInfo(mi.InfoBytes),
				torrent.OptionTrackers(tracker.URL+"/announce"),
			)
			require.NoError(t, err)

			leechtorrent, _, err := leecher.Start(leechmd)
			require.NoError(t, err)

			var downloaded bytes.Buffer
			_, err = torrent.DownloadInto(ctx, &downloaded, leechtorrent)
			require.NoError(t, err)
			require.EqualValues(t, testutil.GreetingFileContents, downloaded.String())
			require.NotZero(t, announces.Load(), "the leecher must have found the seeder through the tracker")

			require.Equal(t, !c.seeder, seeddht.Available())
			require.Equal(t, !c.leecher, leechdht.Available())
			if c.leecher {
				// identity is preserved even though the dht is not serving.
				require.Equal(t, leechid, leechdht.ID(torrenttestx.AddrPorts(leecher)[0]))
			}
		})
	}
}
