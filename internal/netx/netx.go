package netx

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/slicesx"
	"golang.org/x/net/proxy"
)

// Dialer missing interface from the net package.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type DialerFn func(ctx context.Context, addr string) (net.Conn, error)

func (n DialerFn) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return n(ctx, addr)
}

// AddrPortPriority returns a sort key for a network address; lower binds first.
// Order: public IPv6 (0) < private/ULA/link-local IPv6 (1) < public IPv4 (2) < everything else (3).
func AddrPortPriority(ap netip.AddrPort) int {
	addr := ap.Addr().Unmap()
	isPublic := addr.IsGlobalUnicast() && !addr.IsPrivate()
	switch {
	case addr.Is6() && isPublic:
		return 0
	case addr.Is6():
		return 1
	case addr.Is4() && isPublic:
		return 2
	default:
		return 3
	}
}

// Reachable reports whether dst is reachable from a socket bound to from.
// Reachability is determined by scope: loopback ↔ loopback only;
// link-local ↔ link-local only; routed addresses (private + public) ↔ each
// other. All scopes are family-locked (IPv4 ↔ IPv6 returns false).
func Reachable(dst netip.AddrPort, from netip.AddrPort) bool {
	d := dst.Addr().Unmap()
	f := from.Addr().Unmap()
	if !d.IsValid() || !f.IsValid() {
		return false
	}

	dScope := addrScope(d)
	if dScope != addrScope(f) {
		return false
	}

	return d.Is4() == f.Is4()
}

func addrScope(addr netip.Addr) int {
	switch {
	case addr.IsLoopback():
		return 0
	case addr.IsLinkLocalUnicast():
		return 1
	default:
		return 2
	}
}

func CmpAddrPort(a, b netip.AddrPort) int {
	ap := netip.AddrPortFrom(netip.AddrFrom16(a.Addr().As16()), a.Port())
	bp := netip.AddrPortFrom(netip.AddrFrom16(b.Addr().As16()), b.Port())
	return ap.Compare(bp)
}

// NetPort returns the port of the network address,
func NetPort(addr net.Addr) (port int, err error) {
	if addr == nil {
		return 0, errorsx.New("NetPort: nil net.Addr received")
	}

	switch raw := addr.(type) {
	case *net.UDPAddr:
		return raw.Port, nil
	case *net.TCPAddr:
		return raw.Port, nil
	default:
		var (
			sport string
			i64   int64
		)

		if _, sport, err = net.SplitHostPort(addr.String()); err != nil {
			return -1, err
		}

		if i64, err = strconv.ParseInt(sport, 0, 0); err != nil {
			return -1, err
		}

		return int(i64), nil
	}
}

// NetIP returns the IP address of the network address.
func NetIP(addr net.Addr) (ip net.IP, err error) {
	if addr == nil {
		return nil, errorsx.New("NetIP: nil net.Addr received")
	}

	switch raw := addr.(type) {
	case *net.UDPAddr:
		return raw.IP, nil
	case *net.TCPAddr:
		return raw.IP, nil
	default:
		var (
			host string
		)

		if host, _, err = net.SplitHostPort(addr.String()); err != nil {
			return nil, err
		}

		if ip = net.ParseIP(host); ip == nil {
			return nil, errorsx.Errorf("invalid IP: %s", host)
		}

		return ip, nil
	}
}

func NetIPOrNil(addr net.Addr) (ip net.IP) {
	ip, err := NetIP(addr)
	if err != nil {
		log.Println(err)
		return nil
	}

	return ip
}

// NetIPPort returns the IP and Port of the network address
func NetIPPort(addr net.Addr) (ip net.IP, port int, err error) {
	if addr == nil {
		return nil, 0, errorsx.New("NetIPPort: nil net.Addr received")
	}

	switch raw := addr.(type) {
	case *net.UDPAddr:
		return raw.IP, raw.Port, nil
	case *net.TCPAddr:
		return raw.IP, raw.Port, nil
	default:
		var (
			host  string
			sport string
			i64   int64
		)

		if host, sport, err = net.SplitHostPort(addr.String()); err != nil {
			return nil, -1, err
		}

		if ip = net.ParseIP(host); ip == nil {
			return nil, -1, errorsx.Errorf("invalid IP: %s", host)
		}

		if i64, err = strconv.ParseInt(sport, 0, 0); err != nil {
			return nil, -1, err
		}

		return ip, int(i64), nil
	}
}

func AddrPort(addr net.Addr) (_ netip.AddrPort, err error) {
	if addr == nil {
		return netip.AddrPort{}, errorsx.New("NetIPPort: nil net.Addr received")
	}

	switch raw := addr.(type) {
	case *net.UDPAddr:
		return raw.AddrPort(), nil
	case *net.TCPAddr:
		return raw.AddrPort(), nil
	default:
		return netip.ParseAddrPort(addr.String())
	}
}

func AddrFromIP(ip net.IP) netip.Addr {
	if ip == nil {
		return netip.IPv6Unspecified().Unmap()
	}

	return netip.AddrFrom16([16]byte(ip.To16())).Unmap()
}

func IP4FromAddr(a netip.Addr) net.IP {
	if a.Is4() || a.Is4In6() {
		tmp := a.As4()
		return tmp[:]
	}

	return nil
}

func IP6FromAddr(a netip.Addr) net.IP {
	if a.Is6() {
		tmp := a.As16()
		return tmp[:]
	}

	return nil
}

func FirstAddrOrZero(addrs ...netip.Addr) netip.Addr {
	for _, a := range addrs {
		if a.IsValid() && !a.IsUnspecified() {
			return a
		}
	}

	return netip.Addr{}
}

func IsAddrInUse(err error) bool {
	return strings.Contains(err.Error(), "address already in use")
}

// debfault the port if its not present in the hostport string.
// assumes: host:port syntax.
func DefaultPort(hostport string, fallback int) string {
	host, _, ok := strings.Cut(hostport, ":")
	if ok {
		return hostport
	}

	return fmt.Sprintf("%s:%d", host, fallback)
}

func ProxyDialer() proxy.ContextDialer {
	return proxyContextDialer(proxy.FromEnvironment())
}

func proxyContextDialer(d proxy.Dialer) proxy.ContextDialer {
	if d, ok := d.(proxy.ContextDialer); ok {
		return d
	}

	return fakecontextdialer{d: d}
}

type fakecontextdialer struct {
	d proxy.Dialer
}

// WARNING: this can leak a goroutine for as long as the underlying Dialer implementation takes to timeout
// A Conn returned from a successful Dial after the context has been cancelled will be immediately closed.
func (t fakecontextdialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var (
		conn net.Conn
		done = make(chan struct{}, 1)
		err  error
	)
	go func() {
		conn, err = t.d.Dial(network, address)
		close(done)
		if conn != nil && ctx.Err() != nil {
			conn.Close()
		}
	}()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-done:
	}
	return conn, err
}

// CmpAddrPortPriority compares two AddrPorts by priority using AddrPortPriority,
// then breaks ties with raw address comparison. Lower value sorts first.
func CmpAddrPortPriority(a, b netip.AddrPort) int {
	pa := AddrPortPriority(a)
	pb := AddrPortPriority(b)
	if pa != pb {
		return pa - pb
	}
	return CmpAddrPort(a, b)
}

// ComputeBestAddr returns the best routable local address for a bound listener.
// When the listener is bound to an unspecified address (0.0.0.0 / ::),
// it enumerates interface addresses and picks the highest-priority one.
func ComputeBestAddr(bound net.Addr) netip.AddrPort {
	ap := errorsx.Zero(AddrPort(bound))
	if !ap.Addr().Unmap().IsUnspecified() {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}

	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return ap
	}

	blen := ap.Addr().Unmap().BitLen()
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
	ips = slicesx.Filter(func(v netip.AddrPort) bool {
		return v.Addr().Unmap().BitLen() == blen
	}, ips...)
	slices.SortStableFunc(ips, CmpAddrPortPriority)
	best := langx.FirstNonZero(ips...)
	// log.Println("best local ip", bound.Network(), bound, ap, best)
	return best
}
