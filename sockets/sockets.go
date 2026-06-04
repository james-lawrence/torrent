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

type config struct {
	computeBestAddr func(net.Addr) netip.AddrPort
}

// Option configures a Socket.
type Option func(*config)

// OptionComputeBestAddr overrides the function used to determine the best
// local address to advertise for this socket. Defaults to netx.ComputeBestAddr.
func OptionComputeBestAddr(fn func(net.Addr) netip.AddrPort) Option {
	return func(c *config) {
		c.computeBestAddr = fn
	}
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
func New(l net.Listener, d netx.Dialer, options ...Option) Socket {
	cfg := config{computeBestAddr: netx.ComputeBestAddr}
	for _, o := range options {
		o(&cfg)
	}

	if l, ok := l.(packetlistener); ok {
		return &packetconnSocket{packetlistener: l, dialer: d, config: cfg}
	}

	return &socket{Listener: l, dialer: d, config: cfg}
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
	config
}

// Addr returns the best available local address for this socket.
func (t *packetconnSocket) Addr() net.Addr {
	if c := t.cached.Load(); c != nil && time.Since(c.ts) <= addrCacheTTL {
		return c.netaddr
	}
	bound := t.packetlistener.Addr()
	ap := t.computeBestAddr(bound)
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
	config
}

// Addr returns the best available local address for this socket.
func (t *socket) Addr() net.Addr {
	if c := t.cached.Load(); c != nil && time.Since(c.ts) <= addrCacheTTL {
		return c.netaddr
	}
	bound := t.Listener.Addr()
	ap := t.computeBestAddr(bound)
	na := addrOf(ap, bound.Network())
	t.cached.Store(&cachedAddr{addr: ap, netaddr: na, ts: time.Now()})
	return na
}

// Dial remote peers from this socket.
func (t *socket) Dial(ctx context.Context, addr string) (conn net.Conn, err error) {
	return t.dialer.DialContext(ctx, t.Listener.Addr().Network(), addr)
}
