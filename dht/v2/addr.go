package dht

import (
	"net"
	"net/netip"

	"github.com/anacrolix/missinggo/v2"

	"github.com/james-lawrence/torrent/dht/v2/krpc"
)

// Used internally to refer to node network addresses. String() is called a
// lot, and so can be optimized. Network() is not exposed, so that the
// interface does not satisfy net.Addr, as the underlying type must be passed
// to any OS-level function that take net.Addr.
type Addr interface {
	Raw() net.Addr
	Port() int
	IP() net.IP
	String() string
	KRPC() krpc.NodeAddr
}

// Speeds up some of the commonly called Addr methods.
type cachedAddr struct {
	raw  net.Addr
	port int
	ip   netip.Addr
	s    string
}

func (ca cachedAddr) String() string {
	return ca.s
}

func (ca cachedAddr) KRPC() krpc.NodeAddr {
	return krpc.NodeAddr{
		IP:   ca.ip,
		Port: ca.port,
	}
}

func (ca cachedAddr) IP() net.IP {
	return ca.ip.AsSlice()
}

func (ca cachedAddr) Port() int {
	return ca.port
}

func (ca cachedAddr) Raw() net.Addr {
	return ca.raw
}

func NewAddr(raw net.Addr) Addr {
	ip, _ := netip.AddrFromSlice(missinggo.AddrIP(raw))
	return cachedAddr{
		raw:  raw,
		s:    raw.String(),
		ip:   ip,
		port: missinggo.AddrPort(raw),
	}
}
