package sockets

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/internal/netx"
)

const addrCacheTTL = 5 * time.Minute

type cachedAddr struct {
	addr    netip.AddrPort
	netaddr net.Addr
	ts      time.Time
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
	ap := netx.ComputeBestAddr(bound)
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
	ap := netx.ComputeBestAddr(bound)
	na := addrOf(ap, bound.Network())
	t.cached.Store(&cachedAddr{addr: ap, netaddr: na, ts: time.Now()})
	return na
}

// Dial remote peers from this socket.
func (t *socket) Dial(ctx context.Context, addr string) (conn net.Conn, err error) {
	return t.dialer.DialContext(ctx, t.Listener.Addr().Network(), addr)
}
