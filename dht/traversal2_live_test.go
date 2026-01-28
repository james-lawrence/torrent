package dht

import (
	"context"
	"iter"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal"
	"github.com/james-lawrence/torrent/dht/traversal2"
	"github.com/james-lawrence/torrent/dht/types"
)

type serverQuerier struct {
	s *Server
}

func (q serverQuerier) Query(ctx context.Context, addr krpc.NodeAddr, target int160.T) traversal2.QueryResult {
	res := q.s.GetPeers(ctx, NewAddr(addr.AddrPort), target, false)
	r := res.Reply.R
	if r == nil {
		return traversal2.QueryResult{}
	}
	return traversal2.QueryResult{
		ResponseFrom: &krpc.NodeInfo{Addr: addr, ID: r.ID},
		Nodes:        r.Nodes,
		Peers:        r.Values,
	}
}

// TestLiveDHTComparison compares traversal2 against the old traversal using a
// live DHT lookup for archlinux-2026.01.01-x86_64.iso.
//
// LIVE_DHT_TEST=1 go test ./dht/ -run TestLiveDHTComparison -v -count=1 -timeout 900s
func TestLiveDHTComparison(t *testing.T) {
	if os.Getenv("LIVE_DHT_TEST") == "" {
		t.Skip("set LIVE_DHT_TEST=1 to run")
	}

	target, err := int160.FromHexEncodedString("1e873cd33f55737aaaefc0c282c428593c16e106")
	require.NoError(t, err)

	srv, err := NewServer(32, OptionBootstrapGlobal)
	require.NoError(t, err)
	backgroundServe(t, srv, mustListen("0.0.0.0:0"))
	defer srv.Close()

	bctx, bcancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer bcancel()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-t.Context().Done():
				return
			case <-ticker.C:
				s := srv.Stats()
				t.Logf("monitor: nodes=%d good=%d outstanding_tx=%d queries_attempted=%d bad=%d",
					s.Nodes, s.GoodNodes, s.OutstandingTransactions, s.OutboundQueriesAttempted, s.BadNodes)
			}
		}
	}()

	_, err = srv.Bootstrap(bctx)
	require.NoError(t, err)

	seeds := srv.Nodes()
	require.NotEmpty(t, seeds, "routing table empty after bootstrap")

	stats := srv.Stats()
	t.Logf("bootstrap: %d seeds, %d good, %d total", len(seeds), stats.GoodNodes, stats.Nodes)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	t.Run("new", func(t *testing.T) {
		var results []krpc.NodeAddr

		tr := traversal2.New(target, serverQuerier{s: srv}, traversal2.WithK(8), traversal2.WithSeeds(seeds...))
		next, done := iter.Pull(tr.Each(ctx))
		defer done()

		start := time.Now()
		for len(results) < 64 {
			p, ok := next()
			if !ok {
				break
			}
			results = append(results, p)
		}
		t.Logf("traversal2: %d peers in %v", len(results), time.Since(start))
		require.NotEmpty(t, results, "traversal2 found no peers")
	})

	t.Run("old", func(t *testing.T) {
		var (
			mu      sync.Mutex
			results []krpc.NodeAddr
		)

		op := traversal.Start(traversal.OperationInput{
			Target: krpc.ID(target.AsByteArray()),
			K:      8,
			DoQuery: func(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
				res := srv.GetPeers(ctx, NewAddr(addr.AddrPort), target, false)
				r := res.Reply.R
				if r == nil {
					return traversal.QueryResult{}
				}
				mu.Lock()
				results = append(results, r.Values...)
				mu.Unlock()
				return traversal.QueryResult{
					ResponseFrom: &krpc.NodeInfo{Addr: addr, ID: r.ID},
					Nodes:        r.Nodes,
				}
			},
		})
		op.AddNodes(types.AddrMaybeIdSliceFromNodeInfoSlice(seeds))

		start := time.Now()
		<-op.Stalled()
		op.Stop()
		<-op.Stopped()

		mu.Lock()
		count := len(results)
		mu.Unlock()

		t.Logf("old traversal: %d peers in %v", count, time.Since(start))
		require.NotEmpty(t, results, "old traversal found no peers")
	})
}
