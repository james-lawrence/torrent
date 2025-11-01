// Package autobind for automically binding on the local server
// this package is only for convience and it's suggested to use
// torrent.NewSocketsBind instead.
package autobind

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
)

// NewDefaultClient setup a client and connect a using defaults settings.
func NewDefaultClient() (c *torrent.Client, err error) {
	rdir := os.TempDir()
	return New().Bind(torrent.NewClient(torrent.NewDefaultClientConfig(
		torrent.NewMetadataCache(rdir),
		storage.NewFile(rdir),
		torrent.ClientConfigBootstrapGlobal,
	)))
}

// Option for configuring autobind.
type Option func(*Autobind)

// DisableUTP disable UTP sockets
func DisableUTP(a *Autobind) {
	a.utpListen = nil
}

// DisableTCP disable TCP sockets
func DisableTCP(a *Autobind) {
	a.DisableTCP = true
}

func DisableIPv6(a *Autobind) {
	a.DisableIPv6 = true
}

// DisableDHT disables DHT. this is the default.
func DisableDHT(a *Autobind) {
	a.EnableDHT = false
}

// EnableDHT enables DHT.
func EnableDHT(a *Autobind) {
	a.EnableDHT = true
}

func UTPListener(fn func(network string, address string) (s sockets.Socket, err error)) func(*Autobind) {
	return func(a *Autobind) {
		a.utpListen = fn
	}
}

// Autobind manages automatically binding a client to available networks.
type Autobind struct {
	// The address to listen for new uTP and TCP bittorrent protocol
	// connections. DHT shares a UDP socket with uTP unless configured
	// otherwise.
	ListenHost  func(network string) string
	ListenPort  int
	DisableIPv4 bool
	DisableIPv6 bool
	DisableTCP  bool
	EnableDHT   bool
	utpListen   func(network string, address string) (s sockets.Socket, err error)
	tcpListen   func(network string, address string) (s sockets.Socket, err error)
}

// New used to automatically listen to available networks
// on the system. limited configuration options. use client.Bind for more
// robust configuration.
func New(options ...Option) Autobind {
	autobind := Autobind{
		ListenHost: func(string) string { return "" },
		ListenPort: 0,
		tcpListen:  listenTCP,
	}

	for _, opt := range options {
		opt(&autobind)
	}

	return autobind
}

var incr int32

// NewLoopback autobind to the loopback device.
func NewLoopback(options ...Option) Autobind {
	return New(func(a *Autobind) {
		a.ListenHost = func(network string) string {
			if strings.Contains(network, "4") {
				id := atomic.AddInt32(&incr, 1) % 254
				return fmt.Sprintf("127.0.0.%d", id)
			}
			return "::1"
		}
		a.ListenPort = 0
	}, langx.Compose(options...))
}

// NewSpecified for use in testing only, panics if invalid host/port.
func NewSpecified(dst string) Autobind {
	var (
		err         error
		port        int
		host, _port string
	)

	if host, _port, err = net.SplitHostPort(dst); err != nil {
		panic(err)
	}

	if port, err = strconv.Atoi(_port); err != nil {
		panic(err)
	}

	return New(func(a *Autobind) {
		a.ListenHost = func(string) string { return host }
		a.ListenPort = port
	})
}

// Bind the client to available networks. consumes the result of NewClient.
func (t Autobind) Bind(cl *torrent.Client, err error) (*torrent.Client, error) {
	var (
		sockets []sockets.Socket
	)

	if err != nil {
		return nil, err
	}

	if sockets, err = listenAll(t.listenNetworks(), t.ListenHost, t.ListenPort); err != nil {
		return nil, err
	}

	for _, s := range sockets {
		n := parseNetworkString(s.Addr().Network())
		if t.peerNetworkEnabled(n) {
			if err = cl.Bind(s); err != nil {
				return nil, err
			}
		}

		if n.UDP && t.EnableDHT {
			if err = cl.BindDHT(s); err != nil {
				return nil, err
			}
		}
	}

	return cl, nil
}

func (t Autobind) Close() error {
	return nil
}

func (t Autobind) listenNetworks() (ns []network) {
	for _, n := range allPeerNetworks {
		if t.listenOnNetwork(n) {
			n.UTPListen = t.utpListen
			n.TCPListen = t.tcpListen
			n.UDP = t.utpListen != nil
			ns = append(ns, n)
		}
	}
	return ns
}

func (t Autobind) listenOnNetwork(n network) (b bool) {
	if n.Ipv4 && t.DisableIPv4 {
		return false
	}

	if n.Ipv6 && t.DisableIPv6 {
		return false
	}

	if n.TCP && t.DisableTCP {
		return false
	}

	if n.UDP && !t.EnableDHT && t.utpListen == nil {
		return false
	}

	if n.UDP && !t.EnableDHT && t.utpListen == nil {
		return false
	}

	return true
}

func (t Autobind) peerNetworkEnabled(n network) bool {
	if t.utpListen == nil && n.UDP {
		return false
	}
	if t.DisableTCP && n.TCP {
		return false
	}
	if t.DisableIPv6 && n.Ipv6 {
		return false
	}
	if t.DisableIPv4 && n.Ipv4 {
		return false
	}
	return true
}

func listen(n network, addr string) (sockets.Socket, error) {
	switch {
	case n.TCP:
		return n.TCPListen(n.String(), addr)
	case n.UDP:
		return n.UTPListen(n.String(), addr)
	default:
		panic(n)
	}
}

func listenAll(networks []network, getHost func(string) string, port int) ([]sockets.Socket, error) {
	if len(networks) == 0 {
		return nil, nil
	}
	var nahs []networkAndHost
	for _, n := range networks {
		nahs = append(nahs, networkAndHost{n, getHost(n.String())})
	}

	for {
		ss, retry, err := listenAllRetry(nahs, port)
		if !retry {
			return ss, err
		}
	}
}

func listenAllRetry(nahs []networkAndHost, port int) (ss []sockets.Socket, retry bool, err error) {
	ss = make([]sockets.Socket, 1, len(nahs))
	portStr := strconv.FormatInt(int64(port), 10)
	ss[0], err = listen(nahs[0].Network, net.JoinHostPort(nahs[0].Host, portStr))
	if err != nil {
		return nil, false, errorsx.Wrap(err, "first listen")
	}
	defer func() {
		if err != nil || retry {
			for _, s := range ss {
				s.Close()
			}
			ss = nil
		}
	}()

	portStr = strconv.FormatInt(int64(errorsx.Zero(netx.AddrPort(ss[0].Addr())).Port()), 10)
	for _, nah := range nahs[1:] {
		s, err := listen(nah.Network, net.JoinHostPort(nah.Host, portStr))
		if err != nil {
			return ss,
				netx.IsAddrInUse(err) && port == 0,
				errorsx.Wrap(err, "subsequent listen")
		}
		ss = append(ss, s)
	}
	return
}

func listenTCP(network, address string) (s sockets.Socket, err error) {
	l, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			l.Close()
		}
	}()

	dialer := netx.ProxyDialer()
	return sockets.New(l, dialer), nil
}

type networkAndHost struct {
	Network network
	Host    string
}
