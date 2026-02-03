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

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/transactions"
	"github.com/james-lawrence/torrent/internal/netx"
)

type logging interface {
	Println(v ...any)
	Printf(format string, v ...any)
	Print(v ...any)
}

func defaultQueryResendDelay() time.Duration {
	// This should be the highest reasonable UDP latency an end-user might have.
	return 2 * time.Second
}

type transactionKey = transactions.Key

type StartingNodesGetter func(ctx context.Context, dcache dnscacher) ([]Addr, error)

type PeerAnnounce interface {
	Announced(peerid int160.T, ip net.IP, port uint16, portOk bool)
}

type HookQuery func(query *krpc.Msg, source net.Addr) (propagate bool)
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

type Option func(*Server)

func OptionNodeID(id int160.T) Option {
	return func(sc *Server) {
		sc.id.Store(&id)
	}
}

func OptionDynamicPort(fn PublicAddrPort) Option {
	return func(sc *Server) {
		sc.resolvepublicaddr = fn
	}
}

func OptionUPnP(sc *Server) {
	sc.resolvepublicaddr = func(ctx context.Context, id int160.T, local net.PacketConn) (iter.Seq[netip.AddrPort], error) {
		addr, err := netx.AddrPort(local.LocalAddr())
		if err != nil {
			return nil, err
		}

		return UPnPPortForward(ctx, id.String(), addr.Port(), addr)
	}
}

func OptionBootstrapNodesFn(fn StartingNodesGetter) Option {
	return func(sc *Server) {
		sc.bootstrap = append(sc.bootstrap, fn)
	}
}

func OptionBootstrapGlobal(sc *Server) {
	OptionBootstrapNodesFn(GlobalBootstrapAddrs)(sc)
}

func OptionBootstrapPeerFile(path string) Option {
	return OptionBootstrapNodesFn(func(ctx context.Context, dcache dnscacher) (res []Addr, err error) {
		ps, err := ReadNodesFromFile(path)
		if err != nil {
			return nil, err
		}

		for _, p := range ps {
			res = append(res, NewAddr(p.Addr.AddrPort))
		}

		return res, nil
	})
}

func OptionBootstrapNodesNone(sc *Server) {
	sc.bootstrap = nil
}

func OptionBootstrapFixedAddrs(addrs ...Addr) Option {
	return OptionBootstrapNodesFn(func(ctx context.Context, dcache dnscacher) (res []Addr, err error) {
		return addrs, nil
	})
}

func OptionMuxer(m Muxer) Option {
	return func(sc *Server) {
		sc.mux = m
	}
}

func OptionHostResolver(m dnscacher) Option {
	return func(sc *Server) {
		sc.dnscache = m
	}
}

func OptionOnQuery(m HookQuery) Option {
	return func(sc *Server) {
		sc.hookQuery = m
	}
}
func OptionQueryResendDelay(d time.Duration) Option {
	return func(s *Server) {
		s.queryResendDelay = func() time.Duration { return d }
	}
}

func OptionNoop(sc *Server) {}

func OptionLogger(m logging) Option {
	return func(sc *Server) {
		sc.log = m
	}
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
func GlobalBootstrapAddrs(ctx context.Context, dns dnscacher) ([]Addr, error) {
	return ResolveHostPorts(ctx, dns, DefaultGlobalBootstrapHostPorts)
}

// Resolves host:port strings to dht.Addrs, using the dht DNS resolver cache. Suitable for use with
// ServerConfig.BootstrapAddrs.
func ResolveHostPorts(ctx context.Context, dns dnscacher, hostPorts []string) (addrs []Addr, err error) {
	for _, s := range hostPorts {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			log.Println("failed to split host:port", s, err)
			continue
		}

		hostAddrs, err := dns.LookupHost(ctx, host)
		if err != nil {
			log.Println("failed to lookup host addresses", s, err)
			continue
		}
		for _, a := range hostAddrs {
			ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(a, port))
			if err != nil {
				log.Printf("error resolving %q: %v", a, err)
				continue
			}
			addrs = append(addrs, NewAddr(ua.AddrPort()))
		}
	}

	if len(addrs) == 0 {
		return nil, errors.New("nothing resolved")
	}

	return addrs, nil
}
