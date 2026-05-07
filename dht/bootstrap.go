package dht

import (
	"context"
	"errors"
	"time"

	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal"
	"github.com/james-lawrence/torrent/internal/langx"
)

type TraversalStats = traversal.Stats

// Bootstrap populates the routing table for this binding.
func (b *socketbinding) Bootstrap(ctx context.Context, s *Server) (_zero TraversalStats, err error) {
	s.mu.Lock()
	if b.bootstrappingNow {
		s.mu.Unlock()
		return _zero, errors.New("already bootstrapping")
	}
	b.bootstrappingNow = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		b.bootstrappingNow = false
	}()

	id := b.ID()
	t := traversal.Start(traversal.OperationInput{
		Target: id.AsByteArray(),
		K:      64,
		DoQuery: func(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
			return FindNode(ctx, s, NewAddr(addr.AddrPort), s.ID(addr.AddrPort).AsByteArray(), langx.Zero(&id), s.defaultWant).TraversalQueryResult(addr)
		},
		NodeFilter: s.TraversalNodeFilter,
	})

	nodes, err := s.TraversalStartingNodes()
	if err != nil {
		return _zero, err
	}

	t.AddNodes(nodes)
	s.mu.Lock()
	s.lastBootstrap = time.Now()
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-t.Stalled():
	}
	t.Stop()
	if err != nil {
		return _zero, err
	}
	<-t.Stopped()
	return *t.Stats(), nil
}

// Bootstrap populates all binding routing tables.
func (s *Server) Bootstrap(ctx context.Context) (_zero TraversalStats, err error) {
	s.mu.RLock()
	bindings := make([]*socketbinding, len(s.bindings))
	copy(bindings, s.bindings)
	s.mu.RUnlock()

	for _, b := range bindings {
		stats, berr := b.Bootstrap(ctx, s)
		if berr != nil {
			err = berr
			continue
		}
		_zero = stats
	}
	return _zero, err
}
