package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"

	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/stringsx"
	"github.com/james-lawrence/torrent/metainfo"
)

type ClientOperation func(*Client) error

func ClientOperationClearIdleTorrents(idle func(Stats) bool) ClientOperation {
	return func(c *Client) error {
		c.torrents._mu.Lock()
		defer c.torrents._mu.Unlock()
		for id, t := range c.torrents.torrents {
			if !idle(t.Stats()) {
				continue
			}

			errorsx.Log(errorsx.Wrapf(c.Stop(t.md), "failed to shutdown idle torrent: %s", id))
		}

		return nil
	}
}

// Client contain zero or more Torrents. A Client manages a blocklist, the
// TCP/UDP protocol ports, and DHT as desired.
type Client struct {
	// An aggregate of stats over all connections. First in struct to ensure
	// 64-bit alignment of fields. See #262.
	stats ConnStats

	dynamicaddr atomic.Pointer[netip.AddrPort]
	_mu         *sync.RWMutex
	event       sync.Cond
	closed      chan struct{}

	config *ClientConfig

	onClose    []func()
	conns      []sockets.Socket
	dhtServers []*dht.Server

	dialing  *netx.RacingDialer
	torrents *memoryseeding
}

// Query torrent info from the dht
func (cl *Client) Info(ctx context.Context, m Metadata, options ...Tuner) (i *metainfo.Info, err error) {
	var (
		t     *torrent
		added bool
	)

	if t, added, err = cl.start(m); err != nil {
		return nil, err
	} else if !added {
		return nil, fmt.Errorf("attempting to require the info for a torrent that is already running")
	}
	defer cl.Stop(m)

	t.Tune(options...)

	select {
	case <-t.GotInfo():
		return t.Info(), cl.Stop(m)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Start the specified torrent.
// Start adds starts up the torrent within the client downloading the missing pieces
// as needed. if you want to wait until the torrent is completed use Download.
func (cl *Client) Start(t Metadata, options ...Tuner) (dl Torrent, added bool, err error) {
	dl, added, err = cl.start(t, options...)
	if err != nil {
		return dl, added, err
	}

	return dl, added, nil
}

// MaybeStart is a convience method that consumes the return types of the torrent
// creation methods: New, NewFromFile, etc. it is all respects identical to the Start
// method.
func (cl *Client) MaybeStart(t Metadata, failed error, options ...Tuner) (dl Torrent, added bool, err error) {
	if failed != nil {
		return dl, false, failed
	}

	dl, added, err = cl.start(t, options...)
	if err != nil {
		return dl, added, err
	}

	return dl, added, nil
}

func (cl *Client) start(md Metadata, options ...Tuner) (dlt *torrent, added bool, err error) {
	dlt, cached, err := cl.torrents.Load(cl, int160.FromByteArray(md.ID), tuneMerge(md), langx.Compose(options...))
	if errorsx.Ignore(err, fs.ErrNotExist) != nil {
		return nil, false, err
	}

	if cached {
		return dlt, false, nil
	}

	if dlt, err = cl.torrents.Insert(cl, md, tuneMerge(md), langx.Compose(options...)); err != nil {
		return nil, false, err
	}

	cl.AddDHTNodes(dlt.md.DHTNodes)

	cl.lock()
	defer cl.unlock()

	cl.eachDhtServer(func(s *dht.Server) {
		go dlt.dhtAnnouncer(s)
	})

	dlt.updateWantPeersEvent()

	// Tickle Client.waitAccept, new torrent may want conns.
	cl.event.Broadcast()

	return dlt, true, nil
}

// Stop the specified torrent, this halts all network activity around the torrent
// for this client.
func (cl *Client) Stop(t Metadata) (err error) {
	return cl.torrents.Drop(int160.FromByteArray(t.ID))
}

// PeerID ...
func (cl *Client) PeerID() int160.T {
	return cl.config.localID
}

// LocalPort2 returns the local port being listened on.
// WARNING: this method can panic.
// this is method is odd given a client can be attached to multiple ports on different
// listeners.
func (cl *Client) LocalPort16() (port uint16) {
	if port = langx.Autoderef(cl.dynamicaddr.Load()).Port(); port > 0 {
		return port
	}

	cl.eachListener(func(l sockets.Socket) bool {
		addr, err := netx.AddrPort(l.Addr())
		if err != nil {
			log.Println("unable to determine port from listener", err)
			return false
		}
		if addr.Port() == 0 {
			panic(l)
		}
		if port == 0 {
			port = addr.Port()
		}
		return true
	})

	return port
}

func (cl *Client) LocalPort() (port int) {
	return int(cl.LocalPort16())
}

// NewClient create a new client from the provided config. nil is acceptable.
func NewClient(cfg *ClientConfig) (_ *Client, err error) {
	if cfg == nil {
		rootdir := filepath.Join(".", "torrents")
		cfg = NewDefaultClientConfig(
			metadatafilestore{root: rootdir},
			storage.NewFile(rootdir),
			ClientConfigBootstrapGlobal,
		)
	}

	cl := &Client{
		config:   cfg,
		closed:   make(chan struct{}),
		torrents: NewCache(cfg.defaultMetadata, NewBitmapCache(cfg.defaultCacheDirectory)),
		_mu:      &sync.RWMutex{},
		dialing:  netx.NewRacing(cfg.dialPoolSize), // four concurrent dials per cpu seems a reasonable starting point.
	}
	cl.event.L = cl.locker()

	defer func() {
		if err != nil {
			cl.Close()
		}
	}()

	if cfg.localID, err = int160.RandomPrefixed(stringsx.Default(cfg.PeerID, cfg.Bep20)); err != nil {
		return nil, errorsx.Wrap(err, "error generating peer id")
	}

	return cl, nil
}

func (cl *Client) newDhtServer(conn net.PacketConn) (s *dht.Server, err error) {
	cfg := dht.ServerConfig{
		OnAnnouncePeer: cl.onDHTAnnouncePeer,
		PublicIP: func() net.IP {
			if connIsIpv6(conn) && cl.config.publicIP6 != nil {
				return cl.config.publicIP6
			}
			return cl.config.publicIP4
		}(),
		StartingNodes: func() (res []dht.Addr, err error) {
			for _, fn := range cl.config.dhtStartingNodes {
				_local, _err := fn(conn.LocalAddr().Network())()
				res = append(res, _local...)
				err = errorsx.Compact(err, _err)
			}

			return res, err
		},
		OnQuery:     cl.config.DHTOnQuery,
		Logger:      newlogger(cl.config.Logger, "dht", log.Flags()),
		BucketLimit: cl.config.bucketLimit,
	}

	if s, err = dht.NewServer(&cfg); err != nil {
		return s, err
	}

	go func() {
		if err = s.ServeMux(context.Background(), conn, cl.config.DHTMuxer); err != nil {
			log.Println("dht failed", err)
		}
	}()

	go func() {
		ts, err := s.Bootstrap(context.Background())
		if err != nil {
			cl.config.errors().Println(errorsx.Wrap(err, "error bootstrapping dht"))
		}
		cl.config.debug().Printf("%v completed bootstrap (%v)\n", s, ts)
	}()
	return s, nil
}

// Config underlying configuration for the client.
func (cl *Client) Config() *ClientConfig {
	return cl.config
}

// Bind the socket to this client.
func (cl *Client) Bind(s sockets.Socket) (err error) {
	// Check for panics.
	cl.LocalPort()

	go cl.forwardPort()
	go cl.acceptConnections(s)

	cl.lock()
	cl.conns = append(cl.conns, s)
	cl.unlock()

	return nil
}

func (cl *Client) BindDHT(s sockets.Socket) (err error) {
	var (
		ok bool
		pc net.PacketConn
	)

	if pc, ok = s.(net.PacketConn); !ok {
		cl.config.debug().Println("dht servers disabled: not a packet conn")
		return nil
	}

	cl.config.debug().Println("dht servers enabled")

	ds, err := cl.newDhtServer(pc)
	if err != nil {
		return err
	}

	cl.dhtServers = append(cl.dhtServers, ds)

	return nil
}

// Closed returns a channel to detect when the client is closed.
func (cl *Client) Closed() <-chan struct{} {
	cl.rLock()
	defer cl.rUnlock()
	return cl.closed
}

func (cl *Client) eachDhtServer(f func(*dht.Server)) {
	for _, ds := range cl.dhtServers {
		f(ds)
	}
}

func (cl *Client) closeSockets() {
	cl.eachListener(func(l sockets.Socket) bool {
		l.Close()
		return true
	})
}

func (cl *Client) Tune(ops ...ClientOperation) error {
	for _, op := range ops {
		if err := op(cl); err != nil {
			return err
		}
	}

	return nil
}

// Close stops the client. All connections to peers are closed and all activity will
// come to a halt.
func (cl *Client) Close() error {
	select {
	case <-cl.closed:
	default:
		close(cl.closed)
	}
	cl.eachDhtServer(func(s *dht.Server) { s.Close() })
	cl.closeSockets()

	if err := cl.torrents.Close(); err != nil {
		return errorsx.Wrap(err, "unable to close torrents")
	}

	for _, f := range cl.onClose {
		f()
	}
	cl.event.Broadcast()

	return nil
}

func (cl *Client) acceptConnections(l net.Listener) {
	var (
		err  error
		conn net.Conn
	)

	for {
		select {
		case <-cl.closed:
			if conn != nil {
				conn.Close()
			}
			return
		default:
		}

		if conn, err = cl.config.Handshaker.Accept(l); err != nil {
			cl.config.debug().Println(errorsx.Wrap(err, "error accepting connection"))
			continue
		}

		if !cl.config.acceptRateLimiter.Allow() {
			cl.config.debug().Println("rejecting connection due to rate limits")
			conn.Close()
			continue
		}

		go cl.incomingConnection(conn)
	}
}

func (cl *Client) incomingConnection(nc net.Conn) {
	defer nc.Close()
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetLinger(0)
		tc.SetKeepAlive(true)
	}

	addrport, err := netx.AddrPort(nc.RemoteAddr())
	if err != nil {
		log.Println("ignoring incoming connection", err)
		return
	}

	c := cl.newConnection(nc, false, addrport)
	c.Discovery = peerSourceIncoming
	cl.runReceivedConn(c)
}

func reducedDialTimeout(minDialTimeout, maximum time.Duration, halfOpenLimit int, pendingPeers int) (ret time.Duration) {
	ret = maximum / time.Duration((pendingPeers+halfOpenLimit)/halfOpenLimit)
	return max(ret, minDialTimeout)
}

// Returns nil connection and nil error if no connection could be established
// for valid reasons.
func (cl *Client) establishOutgoingConnEx(ctx context.Context, t *torrent, addr netip.AddrPort, obfuscatedHeader bool) (c *connection, err error) {
	var (
		nc net.Conn
	)

	cl.lock()
	conns := make([]netx.DialableNetwork, 0, len(cl.conns))
	for _, c := range cl.conns {
		conns = append(conns, c)
	}
	cl.unlock()

	if len(conns) == 0 {
		return nil, errorsx.Errorf("unable to dial due to no servers")
	}

	if nc, err = cl.dialing.Dial(ctx, t.dialTimeout(), addr.String(), conns...); err != nil {
		cl.config.debug().Println("dialing failed", t.md.ID, cl.dynamicaddr.Load(), "->", addr, err)
		return nil, err
	}

	cl.config.debug().Println("dialing completed", t.md.ID, cl.dynamicaddr.Load(), "->", addr)
	defer func() {
		if err != nil {
			errorsx.Log(nc.Close())
		}
	}()

	// This is a bit optimistic, but it looks non-trivial to thread this through the proxy code. Set
	// it now in case we close the connection forthwith.
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetLinger(0)
		tc.SetKeepAlive(true)
	}

	dl := time.Now().Add(cl.config.handshakesTimeout)
	if err = nc.SetDeadline(dl); err != nil {
		return nil, err
	}

	c = cl.newConnection(nc, true, addr)
	c.headerEncrypted = obfuscatedHeader

	if err = cl.initiateHandshakes(c, t); err != nil {
		return nil, err
	}

	return c, nil
}

// Returns nil connection and nil error if no connection could be established
// for valid reasons.
func (cl *Client) establishOutgoingConn(ctx context.Context, t *torrent, addr netip.AddrPort) (c *connection, err error) {
	obfuscatedHeaderFirst := cl.config.HeaderObfuscationPolicy.Preferred
	if c, err = cl.establishOutgoingConnEx(ctx, t, addr, obfuscatedHeaderFirst); err == nil {
		return c, nil
	}

	if cl.config.HeaderObfuscationPolicy.RequirePreferred {
		// We should have just tried with the preferred header obfuscation. If it was required,
		// there's nothing else to try.
		return c, err
	}

	// Try again with encryption if we didn't earlier, or without if we did.
	if c, err = cl.establishOutgoingConnEx(ctx, t, addr, !obfuscatedHeaderFirst); err != nil {
		return c, err
	}

	return c, nil
}

// Called to dial out and run a connection. The addr we're given is already
// considered half-open.
func (cl *Client) outgoingConnection(ctx context.Context, t *torrent, p Peer) (err error) {
	var (
		c       *connection
		ps      peerSource = p.Source
		trusted bool       = p.Trusted
	)

	defer func() {
		err = errorsx.StdlibTimeout(err, 300*time.Millisecond, syscall.ECONNRESET)
	}()

	if err = cl.config.dialRateLimiter.Wait(ctx); err != nil {
		t.peers.Attempted(p, nil)
		return errorsx.Wrap(err, "dial rate limit failed")
	}

	if c, err = cl.establishOutgoingConn(ctx, t, p.AddrPort); err != nil {
		t.peers.Attempted(p, nil)
		return errorsx.Wrapf(err, "error establishing connection to %v", p.AddrPort)
	}

	t.peers.Attempted(p, nil)

	c.Discovery = ps
	c.trusted = trusted

	// Since the remote address is almost never the same as the local bind address
	// due to network topologies (NAT, LAN, WAN) we have to detect this situation
	// from the origin of the connection and ban the address we connected to.
	if c.PeerID == cl.config.localID {
		cause := connections.NewBanned(
			c.conn,
			errorsx.Errorf("detected connection to self - %s vs %s - %s", c.PeerID, cl.config.localID, c.conn.RemoteAddr().String()),
		)
		cl.config.Handshaker.Release(
			c.conn,
			cause,
		)
		return cause
	}

	defer t.deleteConnection(c)
	defer t.event.Broadcast()
	defer cl.event.Broadcast()

	return RunHandshookConn(c, t)
}

// Calls f with any secret keys.
func (cl *Client) forSkeys(cb func(skey []byte) []byte) []byte {
	for skey := range cl.torrents.Each() {
		if b := skey.Bytes(); cb(b) != nil {
			return b
		}
	}

	return nil
}

func (cl *Client) initiateHandshakes(c *connection, t *torrent) (err error) {
	var (
		rw io.ReadWriter
	)
	rw = c.rw()

	if c.headerEncrypted {
		rw, c.cryptoMethod, err = pp.EncryptionHandshake{
			Keys:           cl.forSkeys,
			CryptoSelector: cl.config.CryptoSelector,
		}.Outgoing(rw, t.md.ID[:], cl.config.CryptoProvides)

		if err != nil {
			return errorsx.Wrap(err, "encryption handshake failed")
		}
	}
	c.setRW(rw)

	ebits, info, err := pp.Handshake{
		PeerID: cl.config.localID.AsByteArray(),
		Bits:   cl.config.extensionbits,
	}.Outgoing(c.rw(), t.md.ID)

	if err != nil {
		return errorsx.Wrap(err, "bittorrent protocol handshake failure")
	}

	cl.config.debug().Println("initiated outgoing connection", cl.PeerID, "->", int160.FromByteArray(info.PeerID))

	c.PeerExtensionBytes = ebits
	c.PeerID = int160.FromByteArray(info.PeerID)
	c.completedHandshake = time.Now()

	return nil
}

// Do encryption and bittorrent handshakes as receiver.
func (cl *Client) receiveHandshakes(c *connection) (t *torrent, err error) {
	var (
		buffered io.ReadWriter
	)

	encryption := pp.EncryptionHandshake{
		Keys:           cl.forSkeys,
		CryptoSelector: cl.config.CryptoSelector,
	}

	if _, buffered, err = encryption.Incoming(c.rw()); errors.Is(err, io.EOF) {
		cl.config.debug().Println("encryption handshake timedout", err)
		return nil, errorsx.Timedout(err, 0)
	} else if err != nil && cl.config.HeaderObfuscationPolicy.RequirePreferred {
		return t, errorsx.Wrap(err, "connection does not have the required header obfuscation")
	} else if err != nil && buffered == nil {
		cl.config.debug().Println("encryption handshake", err)
		// detect stblib timeouts like io.Timeout.
		return nil, errorsx.StdlibTimeout(err, 0)
	} else if err != nil {
		cl.config.debug().Println("encryption handshake", err)
	}

	ebits, info, err := pp.Handshake{
		PeerID: cl.config.localID.AsByteArray(),
		Bits:   cl.config.extensionbits,
	}.Incoming(buffered)

	if err != nil {
		return nil, connections.NewBanned(c.conn, errorsx.Wrap(err, "invalid handshake failed"))
	}

	// cl.config.debug().Println("received incoming connection", int160.FromByteArray(info.PeerID), "->", int160.FromByteArray(cl.config.localID), c.remoteAddr)

	c.PeerExtensionBytes = ebits
	c.PeerID = int160.FromByteArray(info.PeerID)
	c.completedHandshake = time.Now()

	t, _, err = cl.torrents.Load(cl, int160.FromByteArray(info.Hash))
	if err != nil {
		return nil, err
	}

	return t, nil
}

func (cl *Client) runReceivedConn(c *connection) {
	var (
		timedout errorsx.Timeout
	)

	if err := c.conn.SetDeadline(time.Now().Add(cl.config.handshakesTimeout)); err != nil {
		cl.config.errors().Println(errorsx.Wrap(err, "failed setting handshake deadline"))
		return
	}

	t, err := cl.receiveHandshakes(c)
	if errors.As(err, &timedout) {
		cl.config.debug().Printf("received connection timed out %T - %v\n", err, err)
		cl.config.Handshaker.Release(c.conn, err)
		return
	}

	if err != nil {
		cl.config.Handshaker.Release(c.conn, errorsx.Wrap(err, "error during handshake"))
		return
	}

	if err := RunHandshookConn(c, t); err != nil {
		cl.config.debug().Printf("received connection failed %T - %v\n", err, err)
	}
}

// Handle a file-like handle to some torrent data resource.
type Handle interface {
	io.Reader
	io.Seeker
	io.Closer
	io.ReaderAt
}

// DhtServers returns the set of DHT servers.
func (cl *Client) DhtServers() []*dht.Server {
	return cl.dhtServers
}

// AddDHTNodes adds nodes to the DHT servers.
func (cl *Client) AddDHTNodes(nodes []string) {
	for _, n := range nodes {
		addrport, err := netip.ParseAddrPort(n)
		if err != nil {
			cl.config.info().Printf("refusing to add DHT node with invalid IP: %q - %v\n", addrport.Addr(), err)
			continue
		}

		ni := krpc.NodeInfo{
			Addr: krpc.NewNodeAddrFromAddrPort(addrport),
		}
		cl.eachDhtServer(func(s *dht.Server) {
			s.AddNode(ni)
		})
	}
}

func (cl *Client) newConnection(nc net.Conn, outgoing bool, remoteAddr netip.AddrPort) (c *connection) {
	c = newConnection(cl.config, nc, outgoing, remoteAddr, &cl.config.extensionbits, cl.LocalPort16(), &cl.dynamicaddr)
	c.setRW(connStatsReadWriter{nc, c})
	c.r = &rateLimitedReader{
		l: cl.config.DownloadRateLimiter,
		r: c.r,
	}
	cl.config.debug().Printf("initialized with remote %v (outgoing=%t)\n", remoteAddr, outgoing)
	return c
}

func (cl *Client) onDHTAnnouncePeer(ih metainfo.Hash, ip net.IP, port int, portOk bool) {
	cl.config.DHTAnnouncePeer(ih, ip, port, portOk)
	cl.lock()
	defer cl.unlock()

	t, _, err := cl.torrents.Load(cl, int160.FromByteArray(ih))
	if err != nil {
		log.Println("unable to load torrent for peer announce", err)
		return
	}

	t.addPeers(NewPeerDeprecated(
		int160.Zero(),
		ip,
		port,
		PeerOptionSource(peerSourceDhtAnnouncePeer),
	))
}

func (cl *Client) eachListener(f func(sockets.Socket) bool) {
	for _, s := range cl.conns {
		if !f(s) {
			break
		}
	}
}

func (cl *Client) findListener(f func(net.Listener) bool) (ret net.Listener) {
	cl.eachListener(func(l sockets.Socket) bool {
		ret = l
		return !f(l)
	})
	return
}

func (cl *Client) publicIP(peer netip.Addr) netip.Addr {
	// TODO: Use BEP 10 to determine how peers are seeing us.
	if peer.Is4() {
		return netx.FirstAddrOrZero(
			netx.AddrFromIP(cl.config.publicIP4),
			cl.findListenerIP(func(ip netip.Addr) bool { return ip.Is4() && ip.IsValid() }),
		)
	}

	return netx.FirstAddrOrZero(
		netx.AddrFromIP(cl.config.publicIP6),
		cl.findListenerIP(func(ip netip.Addr) bool { return ip.Is6() && ip.IsValid() }),
	)
}

func (cl *Client) findListenerIP(f func(netip.Addr) bool) netip.Addr {
	l := cl.findListener(func(l net.Listener) bool {
		addr, err := netx.AddrPort(l.Addr())
		if err != nil {
			log.Println("invalid listener", err)
			return false
		}
		return f(addr.Addr())
	})

	if l == nil {
		log.Println("unable to determine listener address/port - no listener found")
		return netip.Addr{}
	}

	addr, err := netx.AddrPort(l.Addr())
	if err != nil {
		log.Println("unable to determine listener address/port", err)
		return netip.Addr{}
	}

	return addr.Addr()
}

// Our IP as a peer should see it.
func (cl *Client) publicAddr(peer netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(
		cl.publicIP(peer.Addr()),
		uint16(cl.LocalPort()),
	)
}

// ListenAddrs addresses currently being listened to.
func (cl *Client) ListenAddrs() (ret []net.Addr) {
	cl.lock()
	defer cl.unlock()
	cl.eachListener(func(l sockets.Socket) bool {
		ret = append(ret, l.Addr())
		return true
	})
	return
}

var _ = atomic.AddInt32

func (cl *Client) rLock() {
	// updated := atomic.AddUint64(&cl.lcount, 1)
	// l2.Output(2, fmt.Sprintf("%p rlock initiated - %d", cl, updated))
	cl._mu.RLock()
	// l2.Output(2, fmt.Sprintf("%p rlock completed - %d", cl, updated))
}

func (cl *Client) rUnlock() {
	// updated := atomic.AddUint64(&cl.ucount, 1)
	// l2.Output(2, fmt.Sprintf("%p runlock initiated - %d", cl, updated))
	cl._mu.RUnlock()
	// l2.Output(2, fmt.Sprintf("%p runlock completed - %d", cl, updated))
}

func (cl *Client) lock() {
	// updated := atomic.AddUint64(&cl.lcount, 1)
	// l2.Output(2, fmt.Sprintf("%p lock initiated - %d", cl, updated))
	cl._mu.Lock()
	// l2.Output(2, fmt.Sprintf("%p lock completed - %d", cl, updated))
}

func (cl *Client) unlock() {
	// updated := atomic.AddUint64(&cl.ucount, 1)
	// l2.Output(2, fmt.Sprintf("%p unlock initiated - %d", cl, updated))
	cl._mu.Unlock()
	// l2.Output(2, fmt.Sprintf("%p unlock completed - %d", cl, updated))
}

func (cl *Client) locker() sync.Locker {
	return clientLocker{cl}
}

func (cl *Client) String() string {
	return fmt.Sprintf("<%[1]T %[1]p>", cl)
}

type clientLocker struct {
	*Client
}

func (cl clientLocker) Lock() {
	// updated := atomic.AddUint64(&cl.lcount, 1)
	// l2.Output(2, fmt.Sprintf("%p lock initiated - %d", cl.Client, updated))
	cl._mu.Lock()
	// l2.Output(2, fmt.Sprintf("%p lock completed - %d", cl.Client, updated))
}

func (cl clientLocker) Unlock() {
	// updated := atomic.AddUint64(&cl.ucount, 1)
	// l2.Output(2, fmt.Sprintf("%p unlock initiated - %d", cl.Client, updated))
	cl._mu.Unlock()
	// l2.Output(2, fmt.Sprintf("%p unlock completed - %d", cl.Client, updated))
}
