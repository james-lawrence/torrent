package traversal2

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
)

func addr(ip string, port uint16) krpc.NodeAddr {
	return krpc.NewNodeAddrFromAddrPort(netip.MustParseAddrPort(fmt.Sprintf("%s:%d", ip, port)))
}

func info(id int160.T, a krpc.NodeAddr) krpc.NodeInfo {
	return krpc.NodeInfo{ID: krpc.ID(id.AsByteArray()), Addr: a}
}

func id(b byte) int160.T {
	return int160.FromByteArray([20]byte{b})
}

type mockQuerier struct {
	responses map[string]QueryResult
}

func (m *mockQuerier) Query(_ context.Context, a krpc.NodeAddr, _ int160.T) QueryResult {
	if m.responses == nil {
		return QueryResult{}
	}
	return m.responses[a.String()]
}

func TestNodeLess(t *testing.T) {
	target := id(0x00)

	t.Run("closer node is less", func(t *testing.T) {
		a := Node{Info: info(id(0x01), addr("127.0.0.1", 1000)), Distance: id(0x01).Distance(target)}
		b := Node{Info: info(id(0xFF), addr("127.0.0.2", 1000)), Distance: id(0xFF).Distance(target)}
		require.True(t, nodeLess(a, b))
		require.False(t, nodeLess(b, a))
	})

	t.Run("same distance uses address", func(t *testing.T) {
		a := Node{Info: info(id(0x01), addr("127.0.0.1", 1000)), Distance: id(0x01).Distance(target)}
		b := Node{Info: info(id(0x01), addr("127.0.0.2", 1000)), Distance: id(0x01).Distance(target)}
		require.True(t, nodeLess(a, b))
	})
}

func TestNext(t *testing.T) {
	target := id(0x00)
	seeds := []krpc.NodeInfo{
		info(id(0xFF), addr("127.0.0.1", 1000)),
		info(id(0x01), addr("127.0.0.2", 1000)),
		info(id(0x0F), addr("127.0.0.3", 1000)),
	}
	tr := New(target, &mockQuerier{}, WithSeeds(seeds))

	n1, ok := tr.next()
	require.True(t, ok)
	require.Equal(t, "127.0.0.2:1000", n1.Info.Addr.String())

	n2, ok := tr.next()
	require.True(t, ok)
	require.Equal(t, "127.0.0.3:1000", n2.Info.Addr.String())

	n3, ok := tr.next()
	require.True(t, ok)
	require.Equal(t, "127.0.0.1:1000", n3.Info.Addr.String())

	_, ok = tr.next()
	require.False(t, ok)
}

func TestAddNodes(t *testing.T) {
	target := id(0x00)

	t.Run("adds nodes", func(t *testing.T) {
		tr := New(target, &mockQuerier{})
		tr.addNodes([]krpc.NodeInfo{
			info(id(0x01), addr("127.0.0.1", 1000)),
			info(id(0x02), addr("127.0.0.2", 1000)),
		})
		require.Equal(t, 2, tr.unqueried.Len())
	})

	t.Run("skips already queried", func(t *testing.T) {
		tr := New(target, &mockQuerier{})
		tr.queried[netip.MustParseAddrPort("127.0.0.1:1000")] = struct{}{}
		tr.addNodes([]krpc.NodeInfo{
			info(id(0x01), addr("127.0.0.1", 1000)),
			info(id(0x02), addr("127.0.0.2", 1000)),
		})
		require.Equal(t, 1, tr.unqueried.Len())
	})
}

func TestUpdateClosest(t *testing.T) {
	target := id(0x00)
	tr := New(target, &mockQuerier{}, WithK(2))

	tr.updateClosest(Node{Info: info(id(0xFF), addr("127.0.0.3", 1000)), Distance: id(0xFF).Distance(target)})
	tr.updateClosest(Node{Info: info(id(0x01), addr("127.0.0.1", 1000)), Distance: id(0x01).Distance(target)})
	tr.updateClosest(Node{Info: info(id(0x02), addr("127.0.0.2", 1000)), Distance: id(0x02).Distance(target)})

	require.Equal(t, 2, tr.closest.Len())

	closest := tr.Closest()
	require.Len(t, closest, 2)
	require.Equal(t, "127.0.0.1:1000", closest[0].Info.Addr.String())
	require.Equal(t, "127.0.0.2:1000", closest[1].Info.Addr.String())
}

func TestTraversal(t *testing.T) {
	target := id(0x00)
	seed := info(id(0x10), addr("127.0.0.1", 1000))
	closer := info(id(0x01), addr("127.0.0.2", 1000))
	closest := info(int160.FromByteArray([20]byte{0x00, 0x01}), addr("127.0.0.3", 1000))

	querier := &mockQuerier{
		responses: map[string]QueryResult{
			"127.0.0.1:1000": {ResponseFrom: &seed, Nodes: []krpc.NodeInfo{closer}},
			"127.0.0.2:1000": {ResponseFrom: &closer, Nodes: []krpc.NodeInfo{closest}},
			"127.0.0.3:1000": {ResponseFrom: &closest},
		},
	}

	tr := New(target, querier, WithK(8), WithSeeds([]krpc.NodeInfo{seed}))

	var discovered []Node
	for n := range tr.Each(context.Background()) {
		discovered = append(discovered, n)
	}

	require.NoError(t, tr.Err())
	require.Len(t, discovered, 3)
	require.Equal(t, "127.0.0.3:1000", tr.Closest()[0].Info.Addr.String())
}

func TestTraversalEarlyTermination(t *testing.T) {
	target := id(0x00)
	close1 := info(id(0x01), addr("127.0.0.1", 1000))
	close2 := info(id(0x02), addr("127.0.0.2", 1000))
	far := info(id(0xFF), addr("127.0.0.3", 1000))

	querier := &mockQuerier{
		responses: map[string]QueryResult{
			"127.0.0.1:1000": {ResponseFrom: &close1, Nodes: []krpc.NodeInfo{far}},
			"127.0.0.2:1000": {ResponseFrom: &close2},
		},
	}

	tr := New(target, querier, WithK(2), WithSeeds([]krpc.NodeInfo{close1, close2}))

	count := 0
	for range tr.Each(context.Background()) {
		count++
	}

	require.NoError(t, tr.Err())
	require.Equal(t, 2, count)
	require.Equal(t, 1, tr.unqueried.Len())
}

func TestContextCancellation(t *testing.T) {
	target := id(0x00)
	seed := info(id(0x01), addr("127.0.0.1", 1000))

	querier := &mockQuerier{
		responses: map[string]QueryResult{
			"127.0.0.1:1000": {ResponseFrom: &seed, Nodes: []krpc.NodeInfo{seed}},
		},
	}

	tr := New(target, querier, WithSeeds([]krpc.NodeInfo{seed}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count := 0
	for range tr.Each(ctx) {
		count++
	}

	require.ErrorIs(t, tr.Err(), context.Canceled)
	require.Equal(t, 0, count)
}

func TestEmptySeeds(t *testing.T) {
	tr := New(id(0x00), &mockQuerier{})

	count := 0
	for range tr.Each(context.Background()) {
		count++
	}

	require.NoError(t, tr.Err())
	require.Equal(t, 0, count)
}

func TestNonRespondingNodes(t *testing.T) {
	target := id(0x00)
	seed := info(id(0x10), addr("127.0.0.1", 1000))
	closer := info(id(0x01), addr("127.0.0.2", 1000))

	querier := &mockQuerier{
		responses: map[string]QueryResult{
			"127.0.0.1:1000": {ResponseFrom: &seed, Nodes: []krpc.NodeInfo{closer}},
			"127.0.0.2:1000": {},
		},
	}

	tr := New(target, querier, WithSeeds([]krpc.NodeInfo{seed}))

	var discovered []Node
	for n := range tr.Each(context.Background()) {
		discovered = append(discovered, n)
	}

	require.NoError(t, tr.Err())
	require.Len(t, discovered, 1)
	require.Equal(t, "127.0.0.1:1000", discovered[0].Info.Addr.String())
	require.Len(t, tr.Closest(), 1)
}
