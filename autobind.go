package torrent

import (
	"cmp"
	"errors"
	"slices"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/sockets"
)

// Binder binds network sockets to the client.
type Binder interface {
	// Bind to the given client if err is nil.
	Bind(cl *Client, err error) (*Client, error)
	Close() error
}

type BinderOption func(v *binder)

// BinderOptionDHT serves the dht server on the packet sockets, replacing the socketless
// dht server clients start with.
func BinderOptionDHT(d *dht.Server) BinderOption {
	return func(v *binder) {
		v.dht = d
	}
}

// NewSocketsBind binds a set of sockets to the client.
// it bypasses any disable checks (tcp,udp, ip4/6) from the configuration.
func NewSocketsBind(s ...sockets.Socket) binder {
	return binder{sockets: s}
}

type binder struct {
	dht     *dht.Server
	sockets []sockets.Socket
}

func (t binder) Options(opts ...BinderOption) binder {
	return langx.Clone(t, opts...)
}

func cmpSockets(a, b sockets.Socket) int {
	ap, _ := netx.AddrPort(a.Addr())
	bp, _ := netx.AddrPort(b.Addr())
	return cmp.Compare(netx.AddrPortPriority(ap), netx.AddrPortPriority(bp))
}

// Bind the client to available networks. consumes the result of NewClient.
func (t binder) Bind(cl *Client, err error) (*Client, error) {
	if err != nil {
		return nil, err
	}

	slices.SortStableFunc(t.sockets, cmpSockets)

	if len(t.sockets) == 0 {
		cl.Close()
		return nil, errorsx.Errorf("at least one socket is required")
	}

	// the dht goes first, the client identifies itself with it as soon as it accepts connections.
	cl.BindDHT(t.dht)

	for _, s := range t.sockets {
		if err = cl.Bind(s); err != nil {
			cl.Close()
			return nil, err
		}
	}

	return cl, nil
}

func (t binder) Close() (err error) {
	for _, s := range t.sockets {
		err = errors.Join(err, s.Close())
	}

	return err
}
