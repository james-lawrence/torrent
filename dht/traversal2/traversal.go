package traversal2

import (
	"context"
	"iter"
	"net/netip"
	"time"

	"github.com/google/btree"

	"github.com/james-lawrence/torrent/internal/multiless"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
)

const (
	defaultK            = 8
	defaultQueryTimeout = 30 * time.Second
)

type node struct {
	info     krpc.NodeInfo
	distance int160.T
}

func nodeLess(a, b node) bool {
	var ml multiless.T
	ml.Compare(a.distance.Cmp(b.distance))
	ml.Compare(a.info.Addr.Compare(b.info.Addr))
	return ml.Less()
}

// Traversal performs a DHT traversal toward a target, yielding peers found.
type Traversal struct {
	target       int160.T
	querier      Querier
	k            int
	queryTimeout time.Duration
	unqueried    *btree.BTreeG[node]
	queried      map[netip.AddrPort]struct{}
	closest      *btree.BTreeG[node]
	err          error
}

// New creates a new Traversal.
func New(target int160.T, querier Querier, opts ...Option) *Traversal {
	t := &Traversal{
		target:       target,
		querier:      querier,
		k:            defaultK,
		queryTimeout: defaultQueryTimeout,
		unqueried:    btree.NewG(2, nodeLess),
		queried:      make(map[netip.AddrPort]struct{}),
		closest:      btree.NewG(2, nodeLess),
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

// WithQueryTimeout sets the per-query timeout.
func WithQueryTimeout(d time.Duration) Option {
	return func(t *Traversal) {
		t.queryTimeout = d
	}
}

// WithSeeds adds initial nodes to query.
func WithSeeds(nodes ...krpc.NodeInfo) Option {
	return func(t *Traversal) {
		t.addNodes(nodes)
	}
}

func (t *Traversal) addNodes(nodes []krpc.NodeInfo) {
	for _, n := range nodes {
		if _, ok := t.queried[n.Addr.AddrPort]; ok {
			continue
		}
		distance := n.ID.Int160().Distance(t.target)
		if t.closest.Len() >= t.k {
			farthest, _ := t.closest.Max()
			if distance.Cmp(farthest.distance) > 0 {
				continue
			}
		}
		t.unqueried.ReplaceOrInsert(node{
			info:     n,
			distance: distance,
		})
	}
}

func (t *Traversal) next() (node, bool) {
	return t.unqueried.DeleteMin()
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
			t.queried[n.info.Addr.AddrPort] = struct{}{}
			qctx, qcancel := context.WithTimeout(ctx, t.queryTimeout)
			res := t.querier.Query(qctx, n.info.Addr, t.target)
			qcancel()
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
