package connections

import (
	"errors"
	"net"

	"github.com/james-lawrence/torrent/internal/netx"
)

// Handshaker accepts connections from a net listener and performs
// a handshake to ensure the connection is acceptable.
type Handshaker interface {
	Accept(l net.Listener) (net.Conn, error)
	Release(c net.Conn, cause error) error
}

// NewHandshaker default handshake method.
func NewHandshaker(firewall FirewallStateful) Handshaker {
	return handshaker{
		Firewall: firewall,
	}
}

type handshaker struct {
	Firewall FirewallStateful
}

func (t handshaker) Accept(l net.Listener) (c net.Conn, err error) {
	var (
		rip  net.IP
		port int
		conn net.Conn
	)

	for {
		if conn, err = l.Accept(); err != nil {
			return nil, err
		}

		if rip, port, err = netx.NetIPPort(conn.RemoteAddr()); err != nil {
			conn.Close()
			continue
		}

		if err = t.Firewall.Blocked(rip, port); err != nil {
			conn.Close()
			continue
		}

		return conn, nil
	}
}

func (t handshaker) Release(conn net.Conn, cause error) (err error) {
	var (
		rip  net.IP
		port int
	)

	if rip, port, err = netx.NetIPPort(conn.RemoteAddr()); err != nil {
		return err
	}

	if banned := new(bannedConnection); errors.As(cause, banned) {
		t.Firewall.Inhibit(rip, port, cause)
	}

	return conn.Close()
}
