package dht

import (
	"context"
	_ "crypto/sha1"
	"errors"
	"iter"
	"log"
	"net"
	"net/netip"
	"time"

	"golang.org/x/time/rate"

	"github.com/james-lawrence/torrent/dht/bep44"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	peer_store "github.com/james-lawrence/torrent/dht/peer-store"
	"github.com/james-lawrence/torrent/dht/transactions"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/iplist"
)

func defaultQueryResendDelay() time.Duration {
	// This should be the highest reasonable UDP latency an end-user might have.
	return 2 * time.Second
}

type announcer struct{ PeerAnnounce }
type transactionKey = transactions.Key

type StartingNodesGetter func() ([]Addr, error)

type PeerAnnounce interface {
	Announced(peerid int160.T, ip net.IP, port uint16, portOk bool)
}

type PeerAnnounceFn func(peerid int160.T, ip net.IP, port uint16, portOk bool)

func (t PeerAnnounceFn) Announced(peerid int160.T, ip net.IP, port uint16, portOk bool) {
	t(peerid, ip, port, portOk)
}

type PublicAddrPort func(ctx context.Context, id int160.T, local net.PacketConn) (iter.Seq[netip.AddrPort], error)

func PublicAddrPortFromPacketConn(ctx context.Context, id int160.T, local net.PacketConn) (iter.Seq[netip.AddrPort], error) {
	addr, err := netx.AddrPort(local.LocalAddr())
	if err != nil {
		return nil, err
	}

	return func(yield func(netip.AddrPort) bool) {
		if !yield(addr) {
			return
		}

		// block until context cancels.
		<-ctx.Done()
	}, nil
}

type ServerConfigOption func(*ServerConfig)

func ServerConfigOptionDynamicPort(fn PublicAddrPort) ServerConfigOption {
	return func(sc *ServerConfig) {
		sc.PublicAddrPort = fn
	}
}

func ServerConfigOptionBootstrapNodesFn(fn StartingNodesGetter) ServerConfigOption {
	return func(sc *ServerConfig) {
		sc.bootstrap = append(sc.bootstrap, fn)
	}
}

func ServerConfigOptionBootstrapGlobal(sc *ServerConfig) {
	ServerConfigOptionBootstrapNodesFn(func() ([]Addr, error) { return GlobalBootstrapAddrs("udp") })(sc)
}

func ServerConfigOptionBootstrapPeerFile(path string) ServerConfigOption {
	return ServerConfigOptionBootstrapNodesFn(func() (res []Addr, err error) {
		ps, err := ReadNodesFromFile(path)
		if err != nil {
			return nil, err
		}

		for _, p := range ps {
			res = append(res, NewAddr(p.Addr.UDP()))
		}

		return res, nil
	})
}

func ServerConfigOptionBootstrapNodesNone(sc *ServerConfig) {
	sc.bootstrap = nil
}

func ServerConfigOptionMuxer(m Muxer) ServerConfigOption {
	return func(sc *ServerConfig) {
		sc.mux = m
	}
}

func ServerConfigOptionNoop(sc *ServerConfig) {}

func NewDefaultServerConfig(options ...func(*ServerConfig)) *ServerConfig {
	return langx.Autoptr(langx.Clone(ServerConfig{
		PublicAddrPort:   PublicAddrPortFromPacketConn,
		DefaultWant:      []krpc.Want{krpc.WantNodes, krpc.WantNodes6},
		Store:            bep44.NewMemory(),
		Exp:              2 * time.Hour,
		SendLimiter:      DefaultSendLimiter,
		Logger:           log.Default(),
		BucketLimit:      32,
		mux:              DefaultMuxer(),
		QueryResendDelay: defaultQueryResendDelay,
	}, options...))
}

func ensureDefaultServerConfig(c *ServerConfig) *ServerConfig {
	defaults := NewDefaultServerConfig()
	c = langx.FirstNonNil(c, defaults)
	c.Store = langx.FirstNonNil(c.Store, defaults.Store)
	c.SendLimiter = langx.FirstNonNil(c.SendLimiter, defaults.SendLimiter)
	c.Logger = langx.FirstNonNil(c.Logger, defaults.Logger)
	c.QueryResendDelay = langx.FirstNonNil(c.QueryResendDelay, defaults.QueryResendDelay)
	c.mux = langx.FirstNonNil(c.mux, defaults.mux)
	c.BucketLimit = langx.FirstNonZero(c.BucketLimit, defaults.BucketLimit)
	c.PublicAddrPort = langx.FirstNonNil(c.PublicAddrPort, defaults.PublicAddrPort)
	return c
}

// ServerConfig allows setting up a  configuration of the `Server` instance to be created with
// NewServer.
type ServerConfig struct {
	PublicAddrPort PublicAddrPort
	// number of nodes per bucket
	BucketLimit int
	// Don't respond to queries from other nodes.
	Passive bool

	// used when there are no good nodes to use in the routing table. This might be called any
	// time when there are no nodes, including during bootstrap if one is performed. Typically it
	// returns the resolve addresses of bootstrap or "router" nodes that are designed to kick-start
	// a routing table.
	bootstrap []StartingNodesGetter

	// Initial IP blocklist to use. Applied before serving and bootstrapping
	// begins.
	IPBlocklist iplist.Ranger

	// Hook received queries. Return false if you don't want to propagate to the default handlers.
	OnQuery func(query *krpc.Msg, source net.Addr) (propagate bool)
	// Called when a peer successfully announces to us.
	OnAnnouncePeer PeerAnnounce
	// How long to wait before resending queries that haven't received a response. Defaults to 2s.
	// After the last send, a query is aborted after this time.
	QueryResendDelay func() time.Duration
	// TODO: Expose Peers, to return NodeInfo for received get_peers queries.
	PeerStore peer_store.Interface
	// BEP-44: Storing arbitrary data in the DHT. If not store provided, a default in-memory
	// implementation will be used.
	Store bep44.Store
	// BEP-44: expiration time with non-announced items. Two hours by default
	Exp time.Duration

	// If no Logger is provided, log.Default is used and log.Debug messages are filtered out. Note
	// that all messages without a log.Level, have log.Debug added to them before being passed to
	// this Logger.
	Logger *log.Logger

	DefaultWant []krpc.Want

	SendLimiter *rate.Limiter

	mux Muxer
}

// ServerStats instance is returned by Server.Stats() and stores Server metrics
type ServerStats struct {
	// Count of nodes in the node table that responded to our last query or
	// haven't yet been queried.
	GoodNodes int
	// Count of nodes in the node table.
	Nodes int
	// Transactions awaiting a response.
	OutstandingTransactions int
	// Individual announce_peer requests that got a success response.
	SuccessfulOutboundAnnouncePeerQueries int64
	// Nodes that have been blocked.
	BadNodes                 uint
	OutboundQueriesAttempted int64
}

type Peer = krpc.NodeAddr

var DefaultGlobalBootstrapHostPorts = []string{
	"router.utorrent.com:6881",
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.aelitis.com:6881",     // Vuze
	"router.silotis.us:6881",   // IPv6
	"dht.libtorrent.org:25401", // @arvidn's
	"dht.anacrolix.link:42069",
	"router.bittorrent.cloud:42069",
}

// Returns the resolved addresses of the default global bootstrap nodes. Network is unused but was
// historically passed by anacrolix/torrent.
func GlobalBootstrapAddrs(network string) ([]Addr, error) {
	return ResolveHostPorts(DefaultGlobalBootstrapHostPorts)
}

// Resolves host:port strings to dht.Addrs, using the dht DNS resolver cache. Suitable for use with
// ServerConfig.BootstrapAddrs.
func ResolveHostPorts(hostPorts []string) (addrs []Addr, err error) {
	initDnsResolver()
	for _, s := range hostPorts {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			panic(err)
		}
		hostAddrs, err := dnsResolver.LookupHost(context.Background(), host)
		if err != nil {
			// log.Default.WithDefaultLevel(log.Debug).Printf("error looking up %q: %v", s, err)
			continue
		}
		for _, a := range hostAddrs {
			ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(a, port))
			if err != nil {
				log.Printf("error resolving %q: %v", a, err)
				continue
			}
			addrs = append(addrs, NewAddr(ua))
		}
	}

	if len(addrs) == 0 {
		return nil, errors.New("nothing resolved")
	}

	return addrs, nil
}
