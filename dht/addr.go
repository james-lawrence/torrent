package dht

import (
	"net"
	"net/netip"

	"github.com/james-lawrence/torrent/dht/krpc"
)

// Used internally to refer to node network addresses. String() is called a
// lot, and so can be optimized. Network() is not exposed, so that the
// interface does not satisfy net.Addr, as the underlying type must be passed
// to any OS-level function that take net.Addr.
type Addr interface {
	Raw() net.Addr
	Port() uint16
	IP() net.IP
	String() string
	KRPC() krpc.NodeAddr
	AddrPort() netip.AddrPort
}

// Speeds up some of the commonly called Addr methods.
type cachedAddr struct {
	v   netip.AddrPort
	raw net.Addr
	s   string
}

func (ca cachedAddr) String() string {
	return ca.s
}

func (ca cachedAddr) KRPC() krpc.NodeAddr {
	return krpc.NodeAddr{
		AddrPort: netip.AddrPortFrom(ca.v.Addr(), ca.v.Port()),
	}
}

func (ca cachedAddr) IP() net.IP {
	addr := ca.v.Addr()
	if addr.Is4In6() {
		return net.IP(addr.Unmap().AsSlice())
	}
	return net.IP(addr.AsSlice())
}

func (ca cachedAddr) Port() uint16 {
	return ca.v.Port()
}

func (ca cachedAddr) Raw() net.Addr {
	return ca.raw
}

func (ca cachedAddr) AddrPort() netip.AddrPort {
	return ca.v
}

func NewAddr(v netip.AddrPort) Addr {
	v = netip.AddrPortFrom(netip.AddrFrom16(v.Addr().As16()), v.Port())
	return cachedAddr{
		raw: net.UDPAddrFromAddrPort(v),
		v:   v,
		s:   v.String(),
	}
}
