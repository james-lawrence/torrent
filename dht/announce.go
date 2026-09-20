package dht

// get_peers and announce_peers.

import (
	"context"
	"fmt"
	"net/netip"
	"runtime/trace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	dhtutil "github.com/james-lawrence/torrent/dht/k-nearest-nodes"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal"
)

// Maintains state for an ongoing Announce operation. An Announce is started by calling
// Server.Announce.
type Announce struct {
	Peers chan PeersValues

	server   *Server
	infoHash int160.T // Target

	announcePeerOpts *AnnouncePeerOpts
	scrape           bool

	traversal *traversal.Operation

	peerAnnounced chan struct{}
	closed        chan struct{}
	closer        *sync.Once
	endtranversal *sync.Once
}

func (a *Announce) String() string {
	return fmt.Sprintf("%[1]T %[1]p of %v on %v", a, a.infoHash, a.server)
}

// Returns the number of distinct remote addresses the announce has queried.
func (a *Announce) NumContacted() uint32 {
	return atomic.LoadUint32(&a.traversal.Stats().NumAddrsTried)
}

func (a *Announce) TraversalStats() TraversalStats {
	return *a.traversal.Stats()
}

// Server.Announce option
type AnnounceOpt func(a *Announce)

// Scrape BEP 33 bloom filters in queries.
func Scrape() AnnounceOpt {
	return func(a *Announce) {
		a.scrape = true
	}
}

// Arguments for announce_peer from a Server.Announce.
type AnnouncePeerOpts struct {
	// The node that we're announcing.
	Addressable addressable
	// The peer port should be determined by the receiver to be the source port of the query packet.
	ImpliedPort bool
}

type addressable interface {
	AddrPort(source netip.AddrPort) netip.AddrPort
}

// Finish an Announce get_peers traversal with an announce of a local peer.
func AnnouncePeer(n addressable, implied bool) AnnounceOpt {
	return func(a *Announce) {
		a.announcePeerOpts = &AnnouncePeerOpts{
			Addressable: n,
			ImpliedPort: implied,
		}
	}
}

// Traverses the DHT graph toward nodes that store peers for the infohash, streaming them to the
// caller.
func (s *Server) AnnounceTraversal(ctx context.Context, id int160.T, opts ...AnnounceOpt) (_ *Announce, err error) {
	// a task covers the whole announce, from the traversal through announcing to the closest nodes. it
	// nests under the caller's task, and is ended by the traversal goroutine or on failing to start.
	ctx, task := trace.NewTask(ctx, "dht.announce")
	trace.Logf(ctx, "dht.infohash", "%s", id)

	a := &Announce{
		Peers:         make(chan PeersValues),
		server:        s,
		infoHash:      id,
		peerAnnounced: make(chan struct{}),
		closed:        make(chan struct{}),
		closer:        &sync.Once{},
		endtranversal: &sync.Once{},
	}
	for _, opt := range opts {
		opt(a)
	}
	a.traversal = traversal.Start(traversal.OperationInput{
		Target:     id.AsByteArray(),
		DoQuery:    a.getPeers,
		NodeFilter: s.TraversalNodeFilter,
		DataFilter: func(data any) bool {
			_, ok := data.(string)
			return ok
		},
	})
	nodes, err := s.TraversalStartingNodes()
	if err != nil {
		a.traversal.Stop()
		trace.Log(ctx, "dht.announce", "no starting nodes")
		task.End()
		return
	}
	a.traversal.AddNodes(nodes)
	trace.Logf(ctx, "dht.announce", "starting nodes=%d", len(nodes))
	go func() {
		defer task.End()

		region := trace.StartRegion(ctx, "dht.traversal")
		select {
		case <-a.traversal.Stalled():
			// log.Println("traversal stalled")
			trace.Log(ctx, "dht.announce", "traversal stalled")
		case <-ctx.Done():
			// log.Println("traversal", ctx.Err())
			trace.Log(ctx, "dht.announce", "traversal context done")
		}

		a.traversal.Stop()
		<-a.traversal.Stopped()
		region.End()
		trace.Logf(ctx, "dht.announce", "traversal contacted=%d", a.NumContacted())

		if a.announcePeerOpts != nil {
			region := trace.StartRegion(ctx, "dht.announce_peer")
			a.announceClosest(ctx)
			region.End()
		}

		a.endtranversal.Do(func() {
			close(a.peerAnnounced)
			close(a.Peers)
		})
	}()

	return a, nil
}

func (a *Announce) announceClosest(ctx context.Context) {
	var (
		wg     sync.WaitGroup
		nodes  atomic.Int32
		failed atomic.Int32
	)

	a.traversal.Closest().Range(func(elem dhtutil.Elem) {
		wg.Add(1)
		go func() {
			err := a.announcePeer(ctx, elem)
			a.logger().Printf("announce_peer to %s - %s: %v\n", elem.ID, elem.Addr.AddrPort, err)
			nodes.Add(1)
			if err != nil {
				failed.Add(1)
			}
			wg.Done()
		}()
	})
	wg.Wait()

	// only counts, the addresses of the nodes are not ours to put in a trace.
	trace.Logf(ctx, "dht.announce", "announce_peer nodes=%d failed=%d", nodes.Load(), failed.Load())
}

func (a *Announce) announcePeer(ctx context.Context, peer dhtutil.Elem) error {
	port := a.announcePeerOpts.Addressable.AddrPort(peer.Addr.AddrPort).Port()
	implied := a.announcePeerOpts.ImpliedPort
	if port == 0 && !implied { // nothing to do in this case its invalid just ignore.
		return nil
	}

	ctx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()

	go func() {
		select {
		case <-a.closed:
		case <-ctx.Done():
		}
	}()
	return a.server.announcePeer(
		ctx,
		NewAddr(peer.Addr.AddrPort),
		a.infoHash,
		port,
		peer.Data.(string),
		implied,
	).Err
}

func (a *Announce) getPeers(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
	res := a.server.GetPeers(ctx, NewAddr(addr.AddrPort), a.infoHash, a.scrape)
	if r := res.Reply.R; r != nil && len(r.Values) > 0 {
		peersValues := PeersValues{
			Peers: r.Values,
			NodeInfo: krpc.NodeInfo{
				Addr: addr,
				ID:   r.ID,
			},
			Return: *r,
		}
		select {
		case a.Peers <- peersValues:
		case <-a.traversal.Stopped():
		}
	}
	return res.TraversalQueryResult(addr)
}

// Corresponds to the "values" key in a get_peers KRPC response. A list of
// peers that a node has reported as being in the swarm for a queried info
// hash.
type PeersValues struct {
	Peers         []Peer // Peers given in get_peers response.
	krpc.NodeInfo        // The node that gave the response.
	krpc.Return
}

// Stop the announce.
func (a *Announce) Close() {
	a.StopTraversing()
	a.closer.Do(func() {
		// This will prevent peer announces from proceeding.
		close(a.closed)
	})
}

func (a *Announce) logger() logging {
	return a.server.logger()
}

// Halts traversal, but won't block peer announcing.
func (a *Announce) StopTraversing() {
	a.traversal.Stop()
}

// Traversal and peer announcing steps are done.
func (a *Announce) Finished() <-chan struct{} {
	// This is the last step in an announce.
	return a.peerAnnounced
}
