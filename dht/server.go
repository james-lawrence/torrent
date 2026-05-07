package dht

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"reflect"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"github.com/james-lawrence/torrent/iplist"
	"github.com/james-lawrence/torrent/logonce"
	"golang.org/x/time/rate"

	"github.com/james-lawrence/torrent/dht/bep44"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	peer_store "github.com/james-lawrence/torrent/dht/peer-store"
	"github.com/james-lawrence/torrent/dht/transactions"
	"github.com/james-lawrence/torrent/dht/traversal"
	"github.com/james-lawrence/torrent/dht/types"
	"github.com/james-lawrence/torrent/internal/langx"
)

type dnscacher interface {
	LookupHost(ctx context.Context, host string) (addrs []string, err error)
}

// Binding represents the per-socket identity for one registered DHT socket.
type Binding interface {
	ID() int160.T
	AddrPort() netip.AddrPort
	SendToNode(ctx context.Context, buf []byte, node Addr, maximum int) (bool, error)
	Routing() *table
}

// A Server defines parameters for a DHT node server that is able to send
// queries, and respond to the ones from the network. Each node has a globally
// unique identifier known as the "node ID." Node IDs are chosen at random
// from the same 160-bit space as BitTorrent infohashes and define the
// behaviour of the node. Zero valued Server does not have a valid ID and thus
// is unable to function properly. Use `NewServer(nil)` to initialize a
// default node.
type Server struct {
	k           int
	id          *atomic.Pointer[int160.T]
	dynamicaddr *atomic.Pointer[netip.AddrPort]
	bindings    []*socketbinding

	mu            sync.RWMutex
	transactions  transactions.Dispatcher[*transaction]
	closed        chan struct{}
	tokenServer   tokenServer // Manages tokens we issue to our queriers.
	stats         ServerStats
	announceto    []PeerAnnounce
	dnscache      dnscacher
	lastBootstrap time.Time

	resolvepublicaddr PublicAddrPort
	// Hook received queries. Return false if you don't want to propagate to the default handlers.
	hookQuery HookQuery
	// Called when a peer successfully announces to us.
	hookAnnouncePeer PeerAnnounce
	// How long to wait before resending queries that haven't received a response. Defaults to 2s.
	// After the last send, a query is aborted after this time.
	queryResendDelay func() time.Duration
	defaultWant      []krpc.Want

	// used when there are no good nodes to use in the routing table. This might be called any
	// time when there are no nodes, including during bootstrap if one is performed. Typically it
	// returns the resolve addresses of bootstrap or "router" nodes that are designed to kick-start
	// a routing table.
	bootstrap []StartingNodesGetter

	// Initial IP blocklist to use. Applied before serving and bootstrapping
	// begins.
	blocklist iplist.Ranger
	// TODO: Expose Peers, to return NodeInfo for received get_peers queries.
	peers peer_store.Interface
	// BEP-44: Storing arbitrary data in the DHT.
	store       bep44.Store
	log         logging
	sendLimiter *rate.Limiter

	mux Muxer
}

func (s *Server) numGoodNodes() (num int) {
	for _, b := range s.bindings {
		root := b.ID()
		b.table.forNodes(func(n *node) bool {
			if b.table.isGood(root, n) {
				num++
			}
			return true
		})
	}
	return
}

func (s *Server) numNodes() (num int) {
	for _, b := range s.bindings {
		num += b.table.numNodes()
	}
	return
}

// Stats returns statistics for the server.
func (s *Server) Stats() ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.stats
	ss.GoodNodes = s.numGoodNodes()
	ss.Nodes = s.numNodes()
	ss.OutstandingTransactions = s.transactions.NumActive()
	return ss
}

// DynamicAddrPort returns the server's best-known public address across all bindings.
func (s *Server) DynamicAddrPort() netip.AddrPort {
	if s == nil {
		return netip.AddrPortFrom(netip.IPv6Unspecified(), 0)
	}

	addr := langx.Zero(s.dynamicaddr.Load())

	if ip := addr.Addr(); ip.Is4In6() {
		return netip.AddrPortFrom(ip.Unmap(), addr.Port())
	}

	return addr
}

// AddrPort returns the public address of the binding matching source's address
// family, falling back to the first binding.
func (s *Server) AddrPort(source netip.AddrPort) netip.AddrPort {
	if s == nil {
		return netip.AddrPort{}
	}
	b := s.Binding(source)
	if b == nil {
		return netip.AddrPort{}
	}
	return b.AddrPort()
}

func (s *Server) numBindings() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bindings)
}

type discard struct{}

func (discard) Output(int, string) error {
	return nil
}

// Println replicates the behaviour of the standard logger.
func (t discard) Println(v ...any) {
}

func (t discard) Printf(format string, v ...any) {
}

func (t discard) Print(v ...any) {

}

// NewServer initializes a new DHT node server.
func NewServer(k int, options ...Option) (s *Server, err error) {
	s = langx.Autoptr(langx.Clone(Server{
		k:           k,
		id:          atomicx.Pointer(int160.Random()),
		dynamicaddr: atomicx.Pointer(netip.AddrPortFrom(netip.IPv6Unspecified(), 0)),
		blocklist:   iplist.Zero(),
		tokenServer: tokenServer{
			maxIntervalDelta: 2,
			interval:         5 * time.Minute,
			secret:           make([]byte, 20),
		},
		dnscache:          net.DefaultResolver,
		store:             bep44.NewWrapper(bep44.NewMemory(), 2*time.Hour),
		closed:            make(chan struct{}),
		resolvepublicaddr: PublicAddrPortFromPacketConn,
		sendLimiter:       DefaultSendLimiter,
		mux:               DefaultMuxer(),
		queryResendDelay:  defaultQueryResendDelay,
		defaultWant:       []krpc.Want{krpc.WantNodes, krpc.WantNodes6},
		log:               discard{},
		hookQuery:         func(query *krpc.Msg, source net.Addr) (propagate bool) { return true },
		hookAnnouncePeer:  PeerAnnounceFn(func(peerid int160.T, ip net.IP, port uint16, portOk bool) {}),
	}, options...))

	if _, err = rand.Read(s.tokenServer.secret); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) Binding(addr netip.AddrPort) *socketbinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bindingLocked(addr)
}

func (s *Server) bindingLocked(addr netip.AddrPort) *socketbinding {
	if len(s.bindings) == 0 {
		return nil
	}

	for _, b := range s.bindings {
		if netx.Reachable(addr, langx.Zero(b.dynamicaddr.Load())) {
			return b
		}
	}

	return nil
}

// ServeBinding starts serving on pc and returns the associated Binding once
// the public address is resolved. Callers can use the Binding to observe
// the per-socket node ID.
func (s *Server) ServeBinding(ctx context.Context, pc net.PacketConn, bestaddr netip.AddrPort) (Binding, error) {
	return s.serveBinding(ctx, pc, bestaddr, true)
}

func (s *Server) serveBinding(ctx context.Context, pc net.PacketConn, bestaddr netip.AddrPort, dedup bool) (Binding, error) {
	_dedup := func(pc net.PacketConn) (*socketbinding, bool) {
		incoming := errorsx.Zero(netx.AddrPort(pc.LocalAddr()))
		return slicesx.Find(func(b *socketbinding) bool {
			current := errorsx.Zero(netx.AddrPort(b.pc.LocalAddr()))
			return current.Addr().Compare(incoming.Addr()) == 0
		}, s.bindings...)
	}

	b := &socketbinding{
		// its important that the id different than the actual id so that updateaddr works properly on first pass
		pc:          pc,
		id:          atomicx.Pointer(langx.Zero(s.id.Load())),
		dynamicaddr: atomicx.Pointer(bestaddr),
		table:       newTable(s.k),
		log:         s.log,
	}

	if dedup {
		s.mu.Lock()
		_b, found := _dedup(pc)
		if !found {
			s.bindings = append(s.bindings, b)
		}
		s.mu.Unlock()
		if found {
			return _b, nil
		}
	} else {
		s.mu.Lock()
		s.bindings = append(s.bindings, b)
		s.mu.Unlock()
	}

	updateaddr := func(fixed int160.T, detected netip.AddrPort) {
		latest := fixed.Secure(detected.Addr())
		old := langx.Zero(b.id.Swap(&latest))
		if latest.Cmp(old) == 0 {
			return
		}

		b.logger().Println("binding id changed", bestaddr, detected, old, "->", latest)

		b.dynamicaddr.Store(&detected)
		current := langx.Zero(s.dynamicaddr.Load())

		if current.Port() == 0 || netx.AddrPortPriority(detected) < netx.AddrPortPriority(current) {
			s.dynamicaddr.Store(&detected)
		}
	}

	fixed := langx.Zero(b.id.Load())
	dctx, done := context.WithCancelCause(context.Background())
	seq, err := s.resolvepublicaddr(dctx, s, b, fixed, bestaddr, pc)
	if err != nil {
		s.logger().Println("failed to resolve", err)
		done(nil)
		return nil, err
	}

	for detected := range seq {
		updateaddr(fixed, detected)
		break
	}

	go func() {
		for detected := range seq {
			updateaddr(fixed, detected)
		}
	}()
	go func() {
		done(s.serveUntilClosed(ctx, b))
	}()

	return b, nil
}

func (s *Server) Serve(ctx context.Context, pc net.PacketConn) error {
	bestaddr := netx.ComputeBestAddr(pc.LocalAddr())
	if _, err := s.ServeBinding(ctx, pc, bestaddr); err != nil {
		return err
	}

	// TODO: dualstack socket check instead of just assuming all ip6 is dual stack.
	if bestaddr.Addr().Unmap().Is6() {
		_, err := s.serveBinding(ctx, pc, netx.ComputeBestAddr4(pc.LocalAddr()), false)
		errorsx.Log(errorsx.Wrap(err, "failed to bind ip4 for an dual stack ipv6 socket binding"))
	}

	return nil
}

func (s *Server) isClosed() bool {
	select {
	case _, ok := <-s.closed:
		return !ok
	default:
		return false
	}
}

func (s *Server) AttachAnnouncer(a PeerAnnounce) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.announceto = append(s.announceto, a)
}

func (s *Server) DetachAnnouncer(a PeerAnnounce) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slices.SortStableFunc(s.announceto, func(a, b PeerAnnounce) int {
		aptr := reflect.ValueOf(a).Pointer()
		bptr := reflect.ValueOf(b).Pointer()
		return int(aptr) - int(bptr)
	})
	s.announceto = slices.CompactFunc(s.announceto, func(a, b PeerAnnounce) bool {
		aptr := reflect.ValueOf(a).Pointer()
		bptr := reflect.ValueOf(b).Pointer()
		return aptr == bptr
	})
}

func (s *Server) serveUntilClosed(ctx context.Context, b *socketbinding) error {
	err := s.serve(ctx, b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed() {
		return nil
	}
	return err
}

// Returns a description of the Server.
func (s *Server) String() string {
	return fmt.Sprintf("dht server on %s", langx.Zero(s.dynamicaddr.Load()))
}

// Packets to and from any address matching a range in the list are dropped.
func (s *Server) SetIPBlockList(list iplist.Ranger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocklist = list
}

func (s *Server) IPBlocklist() iplist.Ranger {
	return s.blocklist
}

func (s *Server) processPacket(ctx context.Context, binding *socketbinding, b []byte, addr Addr) {
	// log.Printf("got packet %q", b)
	if len(b) < 2 || b[0] != 'd' {
		// KRPC messages are bencoded dicts.
		readNotKRPCDict.Add(1)
		return
	}
	var d krpc.Msg
	err := bencode.Unmarshal(b, &d)
	if _, ok := err.(bencode.ErrUnusedTrailingBytes); ok {
		// log.Printf("%s: received message packet with %d trailing bytes: %q", s, _err.NumUnusedBytes, b[len(b)-_err.NumUnusedBytes:])
		expvars.Add("processed packets with trailing bytes", 1)
	} else if err != nil {
		readUnmarshalError.Add(1)
		// log.Printf("%s: received bad krpc message from %s: %s: %+q", s, addr, err, b)
		func() {
			if se, ok := err.(*bencode.SyntaxError); ok {
				// The message was truncated.
				if int(se.Offset) == len(b) {
					return
				}
				// Some messages seem to drop to nul chars abruptly.
				if int(se.Offset) < len(b) && b[se.Offset] == 0 {
					return
				}
				// The message isn't bencode from the first.
				if se.Offset == 0 {
					return
				}
			}
			log.Printf("%s: received bad krpc message from %s: %s: %+q", s, addr, err, b)
		}()
		return
	}

	if s.isClosed() {
		return
	}

	if d.Y == krpc.YQuery {
		s.logger().Printf("received query %q from %v\n", d.Q, addr)
		s.handleQuery(ctx, binding, addr, b, d)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tk := transactionKey{
		RemoteAddr: addr.AddrPort(),
		T:          d.T,
	}
	if !s.transactions.Have(tk) {
		s.logger().Printf("received response for untracked transaction %q from %v\n", d.T, addr)
		return
	}
	t := s.transactions.Pop(tk)

	// s.logger().Printf("received response for transaction %q from %v\n", d.T, addr)
	go t.handleResponse(b, d)

	_ = binding.updateNode(addr, d.SenderID(), !d.ReadOnly, func(n *node) {
		n.lastGotResponse = time.Now()
		n.failedLastQuestionablePing = false
		n.numReceivesFrom++
	})
}

func (s *Server) serve(ctx context.Context, binding *socketbinding) error {
	var b [0x10000]byte
	for {
		n, addr, err := binding.pc.ReadFrom(b[:])
		if err != nil {
			if ignoreReadFromError(err) {
				continue
			}
			return err
		}

		if n == len(b) {
			logonce.Stderr.Printf("received dht packet exceeds buffer size")
			continue
		}

		if errorsx.Zero(netx.NetPort(addr)) == 0 {
			readZeroPort.Add(1)
			continue
		}

		blocked, err := func() (bool, error) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			if s.isClosed() {
				return false, errors.New("server is closed")
			}
			return s.ipBlocked(netx.NetIPOrNil(addr)), nil
		}()
		if err != nil {
			return err
		}
		if blocked {
			readBlocked.Add(1)
			continue
		}

		s.processPacket(ctx, binding, b[:n], NewAddr(errorsx.Zero(netx.AddrPort(addr))))
	}
}

func (s *Server) ipBlocked(ip net.IP) (blocked bool) {
	_, blocked = s.blocklist.Lookup(ip)
	return blocked
}

// Adds directly to the node table.
func (s *Server) AddNode(nis ...krpc.NodeInfo) error {
	addnode := func(n krpc.NodeInfo) error {
		addr := NewAddr(n.Addr.AddrPort)
		b := s.Binding(n.Addr.AddrPort)
		if b == nil {
			return nil
		}
		return b.updateNode(addr, &n.ID, true, func(*node) {})
	}

	for _, ni := range nis {
		if id := int160.FromByteArray(ni.ID); id.IsZero() {
			go s.Ping(ni.Addr.AddrPort)
			continue
		}

		if err := addnode(ni); err != nil {
			return err
		}
	}

	return nil
}

func shouldReturnNodes(queryWants []krpc.Want, querySource net.IP) bool {
	if len(queryWants) != 0 {
		return slices.Contains(queryWants, krpc.WantNodes)
	}
	// Is it possible to be over IPv6 with IPv4 endpoints?
	return querySource.To4() != nil
}

func shouldReturnNodes6(queryWants []krpc.Want, querySource net.IP) bool {
	if len(queryWants) != 0 {
		return slices.Contains(queryWants, krpc.WantNodes6)
	}
	return querySource.To4() == nil
}

func (s *Server) MakeReturnNodes(b Binding, target int160.T, filter func(krpc.NodeAddr) bool) []krpc.NodeInfo {
	return s.closestGoodNodeInfos(b, 8, target, filter)
}

var krpcErrMissingArguments = krpc.Error{
	Code: krpc.ErrorCodeProtocolError,
	Msg:  "missing arguments dict",
}

// Filters peers per BEP 32 to return in the values field to a get_peers query.
func filterPeers(querySourceIp net.IP, queryWants []krpc.Want, allPeers []krpc.NodeAddr) (filtered []krpc.NodeAddr) {
	// The logic here is common with nodes, see BEP 32.
	retain4 := shouldReturnNodes(queryWants, querySourceIp)
	retain6 := shouldReturnNodes6(queryWants, querySourceIp)
	for _, peer := range allPeers {
		if ip, ok := func(ip net.IP) (net.IP, bool) {
			as4 := peer.IP().To4()
			as16 := peer.IP().To16()
			switch {
			case retain4 && len(ip) == net.IPv4len:
				return ip, true
			case retain6 && len(ip) == net.IPv6len:
				return ip, true
			case retain4 && as4 != nil:
				// Is it possible that we're converting to an IPv4 address when the transport in use
				// is IPv6?
				return as4, true
			case retain6 && as16 != nil:
				// Couldn't any IPv4 address be converted to IPv6, but isn't listening over IPv6?
				return as16, true
			default:
				return nil, false
			}
		}(peer.IP()); ok {
			filtered = append(filtered, krpc.NewNodeAddrFromIPPort(ip, peer.Port()))
		}
	}
	return
}

func (s *Server) setReturnNodes(b Binding, r *krpc.Return, queryMsg krpc.Msg, querySource Addr) *krpc.Error {
	if queryMsg.A == nil {
		return &krpcErrMissingArguments
	}
	target := int160.FromByteArray(queryMsg.A.InfoHash)
	if shouldReturnNodes(queryMsg.A.Want, querySource.IP()) {
		r.Nodes = s.MakeReturnNodes(b, target, func(na krpc.NodeAddr) bool { return na.Addr().Is4() })
	}
	if shouldReturnNodes6(queryMsg.A.Want, querySource.IP()) {
		r.Nodes6 = s.MakeReturnNodes(b, target, func(krpc.NodeAddr) bool { return true })
	}
	return nil
}

func (s *Server) handleQuery(ctx context.Context, binding *socketbinding, source Addr, raw []byte, m krpc.Msg) {
	var (
		pattern string
		fn      Handler
	)

	_ = binding.updateNode(source, m.SenderID(), !m.ReadOnly, func(n *node) {
		n.lastGotQuery = time.Now()
		n.numReceivesFrom++
	})

	propagate := s.hookQuery(&m, source.Raw())
	if !propagate {
		return
	}

	if pattern, fn = s.mux.Handler(raw, &m); fn == nil {
		log.Println("unable to locate a handler for", pattern)
		return
	}

	if err := fn.Handle(ctx, source, s, binding, raw, &m); err != nil {
		log.Printf("query failed %s - %T - %v\n", source.String(), err, err)
		if cause, ok := err.(krpc.Error); ok {
			if err := s.sendError(ctx, binding, source, m.T, cause); err != nil {
				log.Println("unable to return an error", err)
			}
		}
		if cause, ok := err.(*krpc.Error); ok {
			if err := s.sendError(ctx, binding, source, m.T, *cause); err != nil {
				log.Println("unable to return an error", err)
			}
		}
	}
}

func nodeIsBad(root int160.T, n *node) bool {
	return nodeErr(root, n) != nil
}

func nodeErr(root int160.T, n *node) error {
	// root := langx.Zero(s.id.Load())
	if n.Id == root {
		return errors.New("is self")
	}

	if n.Id.IsZero() {
		return errors.New("has zero id")
	}
	if !(n.IsSecure()) {
		return errors.New("not secure")
	}
	if n.failedLastQuestionablePing {
		return errors.New("didn't respond to last questionable node ping")
	}
	return nil
}

func (s *Server) SendMessageToNode(ctx context.Context, m any, node Addr, maximum int) (wrote bool, err error) {
	b, err := bencode.Marshal(m)
	if err != nil {
		return false, err
	}
	return s.SendToNode(ctx, b, node, maximum)
}

func (s *Server) SendToNode(ctx context.Context, b []byte, node Addr, maximum int) (wrote bool, err error) {
	err = func() error {
		// This is a pain. It would be better if the blocklist returned an error if it was closed
		// instead.
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.isClosed() {
			return errors.New("server is closed")
		}

		if r, ok := s.blocklist.Lookup(node.IP()); ok {
			return fmt.Errorf("write to %v blocked by %v", node, r)
		}

		return nil
	}()

	if err != nil {
		return false, err
	}

	binding := s.Binding(node.AddrPort())
	if binding == nil {
		return false, fmt.Errorf("no socket available: %s", node.AddrPort())
	}
	n, err := repeatsend(ctx, binding.pc, node.Raw(), b, s.queryResendDelay(), maximum)
	if err != nil {
		return false, err
	}

	wrote = true
	if n != len(b) {
		return wrote, io.ErrShortWrite
	}

	return wrote, nil
}

func (s *Server) sendError(ctx context.Context, b Binding, addr Addr, t string, e krpc.Error) error {
	m := krpc.Msg{T: t, Y: krpc.YError, E: &e}
	buf, err := bencode.Marshal(m)
	if err != nil {
		return err
	}
	s.logger().Printf("sending error to %q: %v", addr, e)
	_, err = b.SendToNode(ctx, buf, addr, 1)
	if err != nil {
		s.logger().Printf("error replying to %q: %v", addr, err)
	}
	return err
}

func (s *Server) reply(ctx context.Context, b Binding, addr Addr, t string, r krpc.Return) error {
	r.ID = b.ID().AsByteArray()
	m := krpc.Msg{T: t, Y: krpc.YResponse, R: &r, IP: addr.KRPC()}
	buf := bencode.MustMarshal(m)
	s.logger().Printf("replying to %s\n", addr)
	_, err := b.SendToNode(ctx, buf, addr, 1)
	if err != nil {
		s.logger().Printf("error replying to %s: %s\n", addr, err)
	}
	return err
}

func (s *Server) deleteTransaction(k transactionKey) {
	if s.transactions.Have(k) {
		s.transactions.Pop(k)
	}
}

func (s *Server) addTransaction(k transactionKey, t *transaction) {
	s.transactions.Add(k, t)
}

// ID returns the node ID for the binding matching the given source address.
func (s *Server) ID(addr netip.AddrPort) int160.T {
	b := s.Binding(addr)
	if b == nil {
		return int160.Zero()
	}
	return b.ID()
}

func (s *Server) createToken(addr Addr) string {
	return s.tokenServer.CreateToken(addr)
}

func (s *Server) validToken(token string, addr Addr) bool {
	return s.tokenServer.ValidToken(token, addr)
}

type numWrites int

type QueryResult struct {
	Raw    []byte
	Reply  krpc.Msg
	Writes numWrites
	Err    error
}

func (qr QueryResult) ToError() error {
	if qr.Err != nil {
		return qr.Err
	}

	return nil
}

// Converts a Server QueryResult to a traversal.QueryResult.
func (me QueryResult) TraversalQueryResult(addr krpc.NodeAddr) (ret traversal.QueryResult) {
	r := me.Reply.R
	if r == nil {
		return
	}
	ret.ResponseFrom = &krpc.NodeInfo{
		Addr: addr,
		ID:   r.ID,
	}
	ret.Nodes = r.Nodes
	ret.Nodes6 = r.Nodes6
	if r.Token != nil {
		ret.ClosestData = *r.Token
	}
	return
}

// The zero value for QueryInput uses reasonable/traditional defaults on Server methods.
type QueryInput struct {
	Method   string
	Tid      string
	Encoded  []byte
	NumTries int
}

// Performs an arbitrary query. `q` is the query value, defined by the DHT BEP. `a` should contain
// the appropriate argument values, if any. `a.ID` is clobbered by the Server. Responses to queries
// made this way are not interpreted by the Server. More specific methods like FindNode and GetPeers
// may make use of the response internally before passing it back to the caller.
func (s *Server) Query(ctx context.Context, addr Addr, input QueryInput) (ret QueryResult) {
	defer func(started time.Time) {
		s.logger().Printf(
			"Query(%v) returned after %v (err=%v, reply.Y=%v, reply.E=%v, writes=%v) encoded=%s\n",
			input.Method, time.Since(started), ret.Err, ret.Reply.Y, ret.Reply.E, ret.Writes, base64.URLEncoding.EncodeToString(input.Encoded))
	}(time.Now())

	replyChan := make(chan *QueryResult, 1)
	sctx, done := context.WithCancelCause(pprof.WithLabels(ctx, pprof.Labels("q", input.Method)))
	// Make sure the query sender stops.
	defer done(nil)

	t := &transaction{
		onResponse: func(m []byte, r krpc.Msg) {
			select {
			case replyChan <- &QueryResult{
				Raw:   m,
				Reply: r,
				Err:   r.Error(),
			}:
			case <-sctx.Done():
			}
		},
	}
	tk := transactionKey{
		RemoteAddr: addr.AddrPort(),
	}
	s.mu.Lock()
	s.stats.OutboundQueriesAttempted++
	tk.T = input.Tid
	s.addTransaction(tk, t)
	s.mu.Unlock()

	go func() {
		s.logger().Printf("transmitting initiated %s %s %x %d\n", addr, input.Method, input.Tid, input.NumTries)
		_, err := s.SendToNode(sctx, input.Encoded, addr, input.NumTries)
		s.logger().Printf("transmitting completed %s %s %x %d %v\n", addr, input.Method, input.Tid, input.NumTries, err)
		if err != nil {
			done(err)
		}
	}()

	defer func() {
		s.mu.Lock()
		s.deleteTransaction(tk)
		s.mu.Unlock()
	}()

	select {
	case qr := <-replyChan:
		return *qr
	case <-sctx.Done():
		return NewQueryResultErr(errorsx.Compact(context.Cause(sctx), sctx.Err()))
	}
}

// Sends a ping query to the address given.
func (s *Server) PingQueryInput(ctx context.Context, node netip.AddrPort, qi QueryInput) QueryResult {
	res := PingDuration(ctx, 30*time.Second, s, node, s.ID(node))
	if res.Err == nil {
		id := res.Reply.SenderID()
		if id != nil {
			if b := s.Binding(node); b != nil {
				b.NodeRespondedToPing(NewAddr(node), id.Int160())
			}
		}
	}

	return res
}

// Sends a ping query to the address given.
func (s *Server) Ping(node netip.AddrPort) QueryResult {
	return s.PingQueryInput(context.Background(), node, QueryInput{})
}

func (s *Server) announcePeer(
	ctx context.Context,
	node Addr, infoHash int160.T, port uint16, token string, impliedPort bool,
) (
	ret QueryResult,
) {
	b := s.Binding(node.AddrPort())
	if b == nil {
		return NewQueryResultErr(fmt.Errorf("no socket available: %s", node.AddrPort()))
	}

	qi, err := NewAnnouncePeerRequest(b.ID().AsByteArray(), infoHash.AsByteArray(), port, token, impliedPort)
	if err != nil {
		return NewQueryResultErr(err)
	}

	if ret = s.Query(ctx, node, qi); ret.Err != nil {
		return ret
	}

	if ret.Err != nil {
		return ret
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.SuccessfulOutboundAnnouncePeerQueries++
	return
}

// Returns how many nodes are in the node table.
func (s *Server) NumNodes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.numNodes()
}

// Returns non-bad nodes from the routing table.
func (s *Server) Nodes() (nis []krpc.NodeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		for n := range b.notBadNodes() {
			nis = append(nis, n.NodeInfo())
		}
	}

	return nis
}

// Stops the server network activity. This is all that's required to clean-up a Server.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed() {
		return
	}
	close(s.closed)
	for _, b := range s.bindings {
		go b.pc.Close()
	}
}

func (s *Server) GetPeers(
	ctx context.Context,
	addr Addr,
	infoHash int160.T,
	// Be advised that if you set this, you might not get any "Return.values" back. That wasn't my
	// reading of BEP 33 but there you go.
	scrape bool,
) (ret QueryResult) {
	return FindPeers(ctx, s, addr, s.ID(addr.AddrPort()).AsByteArray(), infoHash.AsByteArray(), scrape)
}

func (s *Server) ClosestGoodNodeInfos(source netip.AddrPort, k int, targetID int160.T) []krpc.NodeInfo {
	b := s.Binding(source)
	if b == nil {
		return nil
	}
	return s.closestGoodNodeInfos(b, k, targetID, func(krpc.NodeAddr) bool { return true })
}

func (s *Server) closestGoodNodeInfos(
	b Binding,
	k int,
	targetID int160.T,
	filter func(krpc.NodeAddr) bool,
) (
	ret []krpc.NodeInfo,
) {
	root := b.ID()
	tbl := b.Routing()
	for _, n := range tbl.closestNodes(root, k, targetID, func(n *node) bool {
		return tbl.isGood(root, n) && filter(n.NodeInfo().Addr)
	}) {
		ret = append(ret, n.NodeInfo())
	}
	return
}

func (s *Server) TraversalStartingNodes() (nodes []types.AddrMaybeId, err error) {
	s.mu.RLock()
	for _, b := range s.bindings {
		b.table.forNodes(func(n *node) bool {
			nodes = append(nodes, n.MaybeId())
			return true
		})
	}
	s.mu.RUnlock()
	if len(nodes) > 0 {
		return nodes, nil
	}

	for _, fn := range s.bootstrap {
		// There seems to be floods on this call on occasion, which may cause a barrage of DNS
		// resolution attempts. This would require that we're unable to get replies because we can't
		// resolve, transmit or receive on the network. Nodes currently don't get expired from the
		// table, so once we have some entries, we should never have to fallback.
		// s.logger().Println("falling back on starting nodes")
		addrs, err := fn(context.Background(), s.dnscache)
		if err != nil {
			return nil, errorsx.Wrap(err, "getting starting nodes")
		}

		for _, a := range addrs {
			nodes = append(nodes, types.AddrMaybeId{Addr: a.KRPC()})
		}
	}

	if len(nodes) == 0 {
		return nil, ErrDHTNoInitialNodes
	}

	return nodes, nil
}

func (s *Server) AddNodesFromFile(fileName string) (added int, err error) {
	ns, err := ReadNodesFromFile(fileName)
	if err != nil {
		log.Println("failed to read peers", err)
		return
	}

	if s.AddNode(ns...) == nil {
		added += len(ns)
	}

	return added, nil
}

func (s *Server) logger() logging {
	return s.log
}

func (s *Server) PeerStore() peer_store.Interface {
	return s.peers
}

func (s *Server) refreshBucket(binding *socketbinding, bucketIndex int) *traversal.Stats {
	id := binding.table.randomIdForBucket(binding.ID(), bucketIndex)
	op := traversal.Start(traversal.OperationInput{
		Target: id.AsByteArray(),
		Alpha:  3,
		// Running this to completion with K matching the full-bucket size should result in a good,
		// full bucket, since the Server will add nodes that respond to its table to replace the bad
		// ones we're presumably refreshing. It might be possible to terminate the traversal early
		// as soon as the bucket is good.
		K: binding.table.K(),
		DoQuery: func(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
			res := FindNode(ctx, s, NewAddr(addr.AddrPort), s.ID(addr.AddrPort).AsByteArray(), id, s.defaultWant)
			if err := res.Err; err != nil && !errors.Is(err, ErrTransactionTimeout) {
				binding.logger().Printf("error doing find node while refreshing bucket: %v\n", err)
			}
			return res.TraversalQueryResult(addr)
		},
		NodeFilter: s.TraversalNodeFilter,
	})
	defer func() {
		op.Stop()
		<-op.Stopped()
	}()
	b := &binding.table.buckets[bucketIndex]
wait:
	for {
		if binding.shouldStopRefreshingBucket(bucketIndex) {
			break wait
		}

		nodes := slicesx.MapTransform(func(n *node) types.AddrMaybeId {
			return n.MaybeId()
		}, slices.Collect(binding.notBadNodes())...)
		op.AddNodes(nodes)
		bucketChanged := b.changed.Signaled()
		select {
		case <-op.Stalled():
			break wait
		case <-bucketChanged:
		case <-s.closed:
		}
	}
	return op.Stats()
}

func (s *Server) shouldBootstrap() bool {
	return s.lastBootstrap.IsZero() || time.Since(s.lastBootstrap) > 30*time.Minute
}

func (s *Server) shouldBootstrapUnlocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shouldBootstrap()
}

// A routine that maintains the Server's routing table, by pinging questionable nodes, and
// refreshing buckets. This should be invoked on a running Server when the caller is satisfied with
// having set it up. It is not necessary to explicitly Bootstrap the Server once this routine has
// started.
func (s *Server) TableMaintainer(ctx context.Context) {
	freq := rate.NewLimiter(rate.Every(5*time.Minute), 1)
	logger := s.logger()
	for {
		if err := freq.Wait(ctx); err != nil {
			log.Println("table maintenance failed", err)
			return
		}

		if s.shouldBootstrapUnlocked() {
			stats, err := s.Bootstrap(ctx)
			if err != nil {
				log.Printf("error bootstrapping during bucket refresh: %v\n", err)
				continue
			}
			logger.Printf("bucket refresh bootstrap stats: %v\n", stats)
		}

		s.mu.RLock()
		bindings := slices.Clone(s.bindings)
		s.mu.RUnlock()
		for _, binding := range bindings {
			for i := range binding.table.buckets {
				binding.pingQuestionableNodesInBucket(s, i)
				if binding.shouldStopRefreshingBucket(i) {
					continue
				}
				logger.Printf("refreshing bucket %v\n", i)
				stats := s.refreshBucket(binding, i)
				logger.Printf("finished refreshing bucket %v: %v\n", i, stats)
				if !binding.shouldStopRefreshingBucket(i) {
					// Presumably we couldn't fill the bucket anymore, so assume we're as deep in the
					// available node space as we can go.
					break
				}
			}
		}
		select {
		case <-s.closed:
			return
		case <-time.After(time.Minute):
		}
	}
}

// Whether we should consider a node for contact based on its address and possible ID.
func (s *Server) TraversalNodeFilter(node types.AddrMaybeId) bool {
	if !validNodeAddr(node.Addr.UDP()) {
		return false
	}

	if s.ipBlocked(node.Addr.IP()) {
		return false
	}

	if !node.Id.Ok {
		return true
	}

	return node.Id.Value.IsSecure(node.Addr.AddrPort.Addr())
}

func validNodeAddr(addr net.Addr) bool {
	// At least for UDP addresses, we know what doesn't work.
	ua := addr.(*net.UDPAddr)
	if ua.Port == 0 {
		return false
	}
	if ip4 := ua.IP.To4(); ip4 != nil && ip4[0] == 0 {
		// Why?
		return false
	}
	return true
}
