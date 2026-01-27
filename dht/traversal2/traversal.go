package traversal2

import (
	"context"
	"iter"
	"net/netip"

	"github.com/anacrolix/multiless"
	"github.com/google/btree"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
)

const (
	defaultK = 8
)

// Node represents a discovered DHT node with its distance to the target.
type Node struct {
	Info     krpc.NodeInfo
	Distance int160.T
}

func nodeLess(a, b Node) bool {
	return multiless.New().Cmp(
		a.Distance.Cmp(b.Distance),
	).Cmp(
		a.Info.Addr.Compare(b.Info.Addr),
	).Less()
}

// Traversal performs a DHT traversal toward a target.
type Traversal struct {
	target    int160.T
	querier   Querier
	k         int
	unqueried *btree.BTreeG[Node]
	queried   map[netip.AddrPort]struct{}
	closest   *btree.BTreeG[Node]
	err       error
}

// New creates a new Traversal.
func New(target int160.T, querier Querier, opts ...Option) *Traversal {
	t := &Traversal{
		target:    target,
		querier:   querier,
		k:         defaultK,
		unqueried: btree.NewG(2, nodeLess),
		queried:   make(map[netip.AddrPort]struct{}),
		closest:   btree.NewG(2, nodeLess),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Option configures a Traversal.
type Option func(*Traversal)

// WithK sets the maximum number of closest nodes to track.
func WithK(k int) Option {
	return func(t *Traversal) {
		t.k = k
	}
}

// WithSeeds adds initial nodes to query.
func WithSeeds(nodes []krpc.NodeInfo) Option {
	return func(t *Traversal) {
		t.addNodes(nodes)
	}
}

func (t *Traversal) addNodes(nodes []krpc.NodeInfo) {
	for _, n := range nodes {
		if _, ok := t.queried[n.Addr.AddrPort]; ok {
			continue
		}
		t.unqueried.ReplaceOrInsert(Node{
			Info:     n,
			Distance: n.ID.Int160().Distance(t.target),
		})
	}
}

func (t *Traversal) next() (Node, bool) {
	n, ok := t.unqueried.Min()
	if !ok {
		return Node{}, false
	}
	if t.closest.Len() >= t.k {
		farthest, _ := t.closest.Max()
		if n.Distance.Cmp(farthest.Distance) > 0 {
			return Node{}, false
		}
	}
	t.unqueried.DeleteMin()
	t.queried[n.Info.Addr.AddrPort] = struct{}{}
	return n, true
}

func (t *Traversal) updateClosest(n Node) {
	t.closest.ReplaceOrInsert(n)
	if t.closest.Len() > t.k {
		t.closest.DeleteMax()
	}
}

// Each returns an iterator that yields discovered nodes.
func (t *Traversal) Each(ctx context.Context) iter.Seq[Node] {
	return func(yield func(Node) bool) {
		for {
			if err := ctx.Err(); err != nil {
				t.err = err
				return
			}
			n, ok := t.next()
			if !ok {
				return
			}
			res := t.querier.Query(ctx, n.Info.Addr, t.target)
			t.addNodes(res.Nodes)
			if res.ResponseFrom == nil {
				continue
			}
			discovered := Node{
				Info:     *res.ResponseFrom,
				Distance: res.ResponseFrom.ID.Int160().Distance(t.target),
			}
			t.updateClosest(discovered)
			if !yield(discovered) {
				return
			}
		}
	}
}

// Err returns any error encountered during iteration.
func (t *Traversal) Err() error {
	return t.err
}

// Closest returns the k-closest nodes found.
func (t *Traversal) Closest() []Node {
	result := make([]Node, 0, t.closest.Len())
	t.closest.Ascend(func(n Node) bool {
		result = append(result, n)
		return true
	})
	return result
}
