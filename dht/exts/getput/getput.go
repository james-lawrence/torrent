package getput

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/james-lawrence/torrent/bencode"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/bep44"
	k_nearest_nodes "github.com/james-lawrence/torrent/dht/k-nearest-nodes"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal"
)

type GetResult struct {
	Seq     int64
	V       bencode.Bytes
	Sig     [64]byte
	Mutable bool
}

func startGetTraversal(
	target bep44.Target, s *dht.Server, seq *int64, salt []byte,
) (
	vChan chan GetResult, op *traversal.Operation, err error,
) {
	vChan = make(chan GetResult)
	op = traversal.Start(traversal.OperationInput{
		Alpha:  15,
		Target: target,
		DoQuery: func(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
			res := s.Get(ctx, dht.NewAddr(addr.UDP()), target, seq, dht.QueryRateLimiting{})
			err := res.ToError()
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, dht.ErrTransactionTimeout) {
				slog.Debug(fmt.Sprintf("error querying %v: %v", addr, err))
			}
			if r := res.Reply.R; r != nil {
				rv := r.V
				bv := rv
				if sha1.Sum(bv) == target {
					select {
					case vChan <- GetResult{
						V:       rv,
						Sig:     r.Sig,
						Mutable: false,
					}:
					case <-ctx.Done():
					}
				} else if sha1.Sum(append(r.K[:], salt...)) == target && bep44.Verify(r.K[:], salt, *r.Seq, bv, r.Sig[:]) {
					select {
					case vChan <- GetResult{
						Seq:     *r.Seq,
						V:       rv,
						Sig:     r.Sig,
						Mutable: true,
					}:
					case <-ctx.Done():
					}
				} else if rv != nil {
					slog.Debug(fmt.Sprintf("get response item hash didn't match target: %q", rv))
				}
			}
			tqr := res.TraversalQueryResult(addr)
			// Filter replies from nodes that don't have a string token. This doesn't look prettier
			// with generics. "The token value should be a short binary string." ¯\_(ツ)_/¯ (BEP 5).
			tqr.ClosestData, _ = tqr.ClosestData.(string)
			if tqr.ClosestData == nil {
				tqr.ResponseFrom = nil
			}
			return tqr
		},
		NodeFilter: s.TraversalNodeFilter,
	})
	nodes, err := s.TraversalStartingNodes()
	op.AddNodes(nodes)
	return
}

func Get(
	ctx context.Context, target bep44.Target, s *dht.Server, seq *int64, salt []byte,
) (
	ret GetResult, stats *traversal.Stats, err error,
) {
	vChan, op, err := startGetTraversal(target, s, seq, salt)
	if err != nil {
		return
	}
	ret.Seq = math.MinInt64
	gotValue := false
receiveResults:
	select {
	case <-op.Stalled():
		if !gotValue {
			err = errors.New("value not found")
		}
	case v := <-vChan:
		slog.Debug(fmt.Sprintf("received %#v", v))
		gotValue = true
		if !v.Mutable {
			ret = v
			break
		}
		if v.Seq >= ret.Seq {
			ret = v
		}
		goto receiveResults
	case <-ctx.Done():
		err = ctx.Err()
	}
	op.Stop()
	stats = op.Stats()
	return
}

type SeqToPut func(seq int64) bep44.Put

func Put(
	ctx context.Context, target krpc.ID, s *dht.Server, salt []byte, seqToPut SeqToPut,
) (
	stats *traversal.Stats, err error,
) {
	vChan, op, err := startGetTraversal(target, s,
		// When we do a get traversal for a put, we don't care what seq the peers have?
		nil,
		// This is duplicated with the put, but we need it to filter responses for autoSeq.
		salt)
	if err != nil {
		return
	}
	var autoSeq int64
notDone:
	select {
	case v := <-vChan:
		if v.Mutable && v.Seq > autoSeq {
			autoSeq = v.Seq
		}
		// There are more optimizations that can be done here. We can set CAS automatically, and we
		// can skip updating the sequence number if the existing content already matches (and
		// presumably republish the existing seq).
		goto notDone
	case <-op.Stalled():
	case <-ctx.Done():
		err = ctx.Err()
	}
	op.Stop()
	var wg sync.WaitGroup
	put := seqToPut(autoSeq)
	op.Closest().Range(func(elem k_nearest_nodes.Elem) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// This is enforced by startGetTraversal.
			token := elem.Data.(string)
			res := s.Put(ctx, dht.NewAddr(elem.Addr.UDP()), put, token, dht.QueryRateLimiting{})
			err := res.ToError()
			if err != nil {
				slog.Warn(fmt.Sprintf("error putting to %v [token=%q]: %v", elem.Addr, token, err))
			} else {
				slog.Warn(fmt.Sprintf("put to %v [token=%q]", elem.Addr, token))
			}
		}()
	})
	wg.Wait()
	stats = op.Stats()
	return
}
