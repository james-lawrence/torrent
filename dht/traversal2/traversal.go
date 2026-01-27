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

type node struct {
	info     krpc.NodeInfo
	distance int160.T
}

func nodeLess(a, b node) bool {
	return multiless.New().Cmp(
		a.distance.Cmp(b.distance),
	).Cmp(
		a.info.Addr.Compare(b.info.Addr),
	).Less()
}

// Traversal performs a DHT traversal toward a target, yielding peers found.
type Traversal struct {
	target    int160.T
	querier   Querier
	k         int
	unqueried *btree.BTreeG[node]
	queried   map[netip.AddrPort]struct{}
	closest   *btree.BTreeG[node]
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
		t.unqueried.ReplaceOrInsert(node{
			info:     n,
			distance: n.ID.Int160().Distance(t.target),
		})
	}
}

func (t *Traversal) next() (node, bool) {
	n, ok := t.unqueried.Min()
	if !ok {
		return node{}, false
	}
	if t.closest.Len() >= t.k {
		farthest, _ := t.closest.Max()
		if n.distance.Cmp(farthest.distance) > 0 {
			return node{}, false
		}
	}
	t.unqueried.DeleteMin()
	t.queried[n.info.Addr.AddrPort] = struct{}{}
	return n, true
}

func (t *Traversal) updateClosest(n node) {
	t.closest.ReplaceOrInsert(n)
	if t.closest.Len() > t.k {
		t.closest.DeleteMax()
	}
}

// Each returns an iterator that yields peers found during traversal.
func (t *Traversal) Each(ctx context.Context) iter.Seq[krpc.NodeAddr] {
	return func(yield func(krpc.NodeAddr) bool) {
		for {
			if err := ctx.Err(); err != nil {
				t.err = err
				return
			}
			n, ok := t.next()
			if !ok {
				return
			}
			res := t.querier.Query(ctx, n.info.Addr, t.target)
			t.addNodes(res.Nodes)
			if res.ResponseFrom != nil {
				t.updateClosest(node{
					info:     *res.ResponseFrom,
					distance: res.ResponseFrom.ID.Int160().Distance(t.target),
				})
			}
			for _, peer := range res.Peers {
				if !yield(peer) {
					return
				}
			}
		}
	}
}

// Err returns any error encountered during iteration.
func (t *Traversal) Err() error {
	return t.err
}
