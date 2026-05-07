package dht

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
)

type socketbinding struct {
	pc               net.PacketConn
	id               *atomic.Pointer[int160.T]
	dynamicaddr      *atomic.Pointer[netip.AddrPort]
	table            *table
	log              logging
	bootstrappingNow bool
}

func (b *socketbinding) logger() logging {
	return b.log
}

func (b *socketbinding) ID() int160.T {
	if b == nil {
		return int160.Zero()
	}
	return langx.Zero(b.id.Load())
}

func (b *socketbinding) AddrPort() netip.AddrPort {
	addr := langx.Zero(b.dynamicaddr.Load())
	if ip := addr.Addr(); ip.Is4In6() {
		return netip.AddrPortFrom(ip.Unmap(), addr.Port())
	}
	return addr
}

func (b *socketbinding) Routing() *table {
	return b.table
}

func (b *socketbinding) SendToNode(ctx context.Context, buf []byte, node Addr, maximum int) (bool, error) {
	n, err := repeatsend(ctx, b.pc, node.Raw(), buf, defaultQueryResendDelay(), maximum)
	if err != nil {
		return false, err
	}
	if n != len(buf) {
		return true, io.ErrShortWrite
	}
	return true, nil
}

func (b *socketbinding) addNode(n *node) error {
	root := langx.Zero(b.id.Load())
	if nodeIsBad(root, n) {
		return errors.New("node is bad")
	}

	bkt := b.table.bucketForID(root, n.Id)
	if excess := b.table.dropN(root, bkt, max(0, bkt.Len()-b.table.k)); excess >= 0 {
		return errors.New("no room in bucket")
	}

	if err := b.table.addNode(root, n); err != nil {
		return fmt.Errorf("expected to add node: %w", err)
	}

	return nil
}

func (b *socketbinding) NodeRespondedToPing(addr Addr, id int160.T) {
	root := langx.Zero(b.id.Load())
	if id == root {
		return
	}

	bkt := b.table.bucketForID(root, id)
	if bkt.GetNode(addr, id) == nil {
		return
	}
	bkt.lastChanged = time.Now()
}

// Returns non-bad nodes from the routing table.
func (b *socketbinding) notBadNodes() iter.Seq[*node] {
	return func(yield func(*node) bool) {
		root := b.ID()
		for n := range b.table.each() {
			if nodeIsBad(root, n) {
				continue
			}

			if !yield(n) {
				return
			}
		}
	}
}

// updateNode updates the node in this binding's routing table, adding it if appropriate.
func (b *socketbinding) updateNode(addr Addr, id *krpc.ID, tryAdd bool, update func(*node)) (err error) {
	if id == nil {
		return errors.New("id is nil")
	}

	if bestaddr := b.AddrPort(); !netx.Reachable(addr.AddrPort(), bestaddr) {
		return fmt.Errorf("node not allowed in this table: %s - %s", bestaddr, addr.AddrPort())
	}

	root := langx.Zero(b.id.Load())
	_id := int160.FromByteArray(*id)
	n := b.table.getNode(root, addr, _id)
	missing := n == nil

	if missing {
		if !tryAdd {
			return errors.New("node not present and add flag false")
		}
		if _id == root {
			return errors.New("can't store own id in routing table")
		}
		n = &node{nodeKey: nodeKey{
			Id:   _id,
			Addr: addr,
		}}
	}

	update(n)

	if !missing {
		return nil
	}

	return b.addNode(n)
}

func (binding *socketbinding) shouldStopRefreshingBucket(bucketIndex int) bool {
	b := &binding.table.buckets[bucketIndex]
	root := binding.ID()
	// Stop if the bucket is full, and none of the nodes are bad.
	return b.Len() == binding.table.K() && b.EachNode(func(n *node) bool {
		return !nodeIsBad(root, n)
	})
}

func (binding *socketbinding) pingQuestionableNodesInBucket(q Queryer, bucketIndex int) {
	b := &binding.table.buckets[bucketIndex]
	root := binding.ID()
	var wg sync.WaitGroup
	b.EachNode(func(n *node) bool {
		if binding.table.isQuestionable(root, n) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, done := context.WithTimeout(context.Background(), 15*time.Second)
				defer done()
				err := binding.questionableNodePing(ctx, q, n.Addr, n.Id.AsByteArray()).Err
				if err != nil {
					binding.logger().Printf("error pinging questionable node in bucket %v: %v", bucketIndex, err)
				}
			}()
		}
		return true
	})
	wg.Wait()
}

func (binding *socketbinding) questionableNodePing(ctx context.Context, q Queryer, addr Addr, id krpc.ID) QueryResult {
	qi, err := NewPingRequest(binding.ID())
	if err != nil {
		return NewQueryResultErr(err)
	}

	// A ping query that will be certain to try at least 3 times.
	qi.NumTries = 3

	res := q.Query(ctx, addr, qi)
	if res.Err == nil && res.Reply.R != nil {
		binding.NodeRespondedToPing(addr, res.Reply.R.ID.Int160())
	} else {
		errorsx.Log(errorsx.Wrap(binding.updateNode(addr, &id, false, func(n *node) {
			n.failedLastQuestionablePing = true
		}), "failed to update questionable node"))
	}
	return res
}
