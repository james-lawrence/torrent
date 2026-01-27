package traversal2_test

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal"
	"github.com/james-lawrence/torrent/dht/traversal2"
	"github.com/james-lawrence/torrent/dht/types"
)

const (
	benchK         = 8
	benchNodeCount = 100
)

type benchQuerier []krpc.NodeInfo

func (q benchQuerier) Query(_ context.Context, addr krpc.NodeAddr, _ int160.T) traversal2.QueryResult {
	idx := int(addr.Port()) - 10000
	if idx < 0 || idx >= len(q) {
		return traversal2.QueryResult{}
	}
	var nodes []krpc.NodeInfo
	if idx+1 < len(q) {
		nodes = append(nodes, q[idx+1])
	}
	if idx+2 < len(q) {
		nodes = append(nodes, q[idx+2])
	}
	peers := []krpc.NodeAddr{
		krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort(fmt.Sprintf("10.0.0.%d:6881", idx))),
	}
	return traversal2.QueryResult{ResponseFrom: &q[idx], Nodes: nodes, Peers: peers}
}

func (q benchQuerier) QueryOld(_ context.Context, addr krpc.NodeAddr) traversal.QueryResult {
	idx := int(addr.Port()) - 10000
	if idx < 0 || idx >= len(q) {
		return traversal.QueryResult{}
	}
	var nodes []krpc.NodeInfo
	if idx+1 < len(q) {
		nodes = append(nodes, q[idx+1])
	}
	if idx+2 < len(q) {
		nodes = append(nodes, q[idx+2])
	}
	return traversal.QueryResult{ResponseFrom: &q[idx], Nodes: nodes}
}

func BenchmarkTraversal(b *testing.B) {
	target := int160.FromByteArray([20]byte{})
	nodes := make(benchQuerier, benchNodeCount)
	for i := range nodes {
		var id [20]byte
		id[0] = byte(i >> 8)
		id[1] = byte(i)
		nodes[i] = krpc.NodeInfo{
			ID:   krpc.ID(id),
			Addr: krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", 10000+i))),
		}
	}
	seeds := nodes[:1]

	b.Run("new", func(b *testing.B) {
		for b.Loop() {
			tr := traversal2.New(target, nodes, traversal2.WithK(benchK), traversal2.WithSeeds(seeds...))
			for range tr.Each(context.Background()) {
			}
		}
	})

	b.Run("old", func(b *testing.B) {
		for b.Loop() {
			op := traversal.Start(traversal.OperationInput{
				Target:  krpc.ID(target.AsByteArray()),
				K:       benchK,
				DoQuery: nodes.QueryOld,
			})
			op.AddNodes(types.AddrMaybeIdSliceFromNodeInfoSlice(seeds))
			<-op.Stalled()
			op.Stop()
			<-op.Stopped()
		}
	})
}
