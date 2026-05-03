package sockets

import (
	"cmp"
	"context"
	"log"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/slicesx"
)

const addrCacheTTL = 5 * time.Minute

type cachedAddr struct {
	addr    netip.AddrPort
	netaddr net.Addr
	ts      time.Time
}

func cmpaddrport(a, b netip.AddrPort) int {
	return cmp.Compare(netx.AddrPortPriority(a), netx.AddrPortPriority(b))
}

// computeBestAddr returns the best routable local address for a bound listener.
// When the listener is bound to an unspecified address (0.0.0.0 / ::),
// it enumerates interface addresses and picks the highest-priority one.
func computeBestAddr(bound net.Addr) netip.AddrPort {
	ap := errorsx.Zero(netx.AddrPort(bound))
	defer log.Println("DERP DERP", ap)
	if !ap.Addr().Unmap().IsUnspecified() {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}

	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return ap
	}

	ips := slicesx.MapTransform(func(n net.Addr) netip.AddrPort {
		switch v := n.(type) {
		case *net.IPNet:
			addr, _ := netip.AddrFromSlice(v.IP)
			return netip.AddrPortFrom(addr.Unmap(), ap.Port())
		case *net.IPAddr:
			addr, _ := netip.AddrFromSlice(v.IP)
			return netip.AddrPortFrom(addr.Unmap(), ap.Port())
		default:
			return netip.AddrPortFrom(netip.Addr{}, ap.Port())
		}
	}, ifaces...)
	slices.SortStableFunc(ips, cmpaddrport)
	return langx.FirstNonZero(ips...)
}

func addrOf(ap netip.AddrPort, network string) net.Addr {
	ip := net.IP(ap.Addr().AsSlice())
	port := int(ap.Port())
	switch network {
	case "udp", "udp4", "udp6":
		return &net.UDPAddr{IP: ip, Port: port}
	default:
		return &net.TCPAddr{IP: ip, Port: port}
	}
}

// New creates a socket from a net listener and dialer.
func New(l net.Listener, d netx.Dialer) Socket {
	if l, ok := l.(packetlistener); ok {
		return &packetconnSocket{packetlistener: l, dialer: d}
	}

	return &socket{Listener: l, dialer: d}
}

// Socket for torrent clients
type Socket interface {
	net.Listener
	Dial(ctx context.Context, addr string) (conn net.Conn, err error)
}

type packetlistener interface {
	net.PacketConn
	// Accept waits for and returns the next connection to the listener.
	Accept() (net.Conn, error)
	// Addr returns the listener's network address.
	Addr() net.Addr
}

type packetconnSocket struct {
	packetlistener
	dialer netx.Dialer
	cached atomic.Pointer[cachedAddr]
}

// Addr returns the best available local address for this socket.
func (t *packetconnSocket) Addr() net.Addr {
	if c := t.cached.Load(); c != nil && time.Since(c.ts) <= addrCacheTTL {
		return c.netaddr
	}
	bound := t.packetlistener.Addr()
	ap := computeBestAddr(bound)
	na := addrOf(ap, bound.Network())
	t.cached.Store(&cachedAddr{addr: ap, netaddr: na, ts: time.Now()})
	return na
}

// Dial remote peers from this socket.
func (t *packetconnSocket) Dial(ctx context.Context, addr string) (conn net.Conn, err error) {
	return t.dialer.DialContext(ctx, t.packetlistener.Addr().Network(), addr)
}

type socket struct {
	net.Listener
	dialer netx.Dialer
	cached atomic.Pointer[cachedAddr]
}

// Addr returns the best available local address for this socket.
func (t *socket) Addr() net.Addr {
	if c := t.cached.Load(); c != nil && time.Since(c.ts) <= addrCacheTTL {
		return c.netaddr
	}
	bound := t.Listener.Addr()
	ap := computeBestAddr(bound)
	na := addrOf(ap, bound.Network())
	t.cached.Store(&cachedAddr{addr: ap, netaddr: na, ts: time.Now()})
	return na
}

// Dial remote peers from this socket.
func (t *socket) Dial(ctx context.Context, addr string) (conn net.Conn, err error) {
	return t.dialer.DialContext(ctx, t.Listener.Addr().Network(), addr)
}
