package torrent

import (
	"context"
	"iter"
	"log"
	"net/netip"
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/upnp"
)

func addPortMapping(d upnp.Device, proto upnp.Protocol, internalPort int, upnpID string) (_zero netip.AddrPort, err error) {
	ctx, done := context.WithTimeout(context.Background(), time.Minute)
	defer done()

	ip, err := d.GetExternalIPv4Address(ctx)
	if err != nil {
		return _zero, errorsx.Wrapf(err, "error adding %s port mapping unable to determined external ip", proto)
	}

	externalPort, err := d.AddPortMapping(ctx, proto, internalPort, internalPort, upnpID, time.Hour)
	if err != nil {
		return _zero, errorsx.Wrapf(err, "error adding %s port mapping", proto)
	}

	return netip.AddrPortFrom(netip.AddrFrom16([16]byte(ip.To16())), uint16(externalPort)), nil
}

func (cl *Client) forwardPort() {
	if cl.config.dynamicip == nil {
		return
	}

	addrs, err := cl.config.dynamicip(context.Background(), cl)
	if err != nil {
		cl.config.errors().Println(err)
		return
	}

	for addrport := range addrs {
		cl.dynamicaddr.Store(&addrport)
		log.Println("dynamic ip update", cl.LocalPort(), "->", addrport)
	}
}

func UPnPPortForward(ctx context.Context, c *Client) (iter.Seq[netip.AddrPort], error) {
	return func(yield func(netip.AddrPort) bool) {
		ds := upnp.Discover(ctx, 0, 2*time.Second)
		c.config.debug().Printf("discovered %d upnp devices\n", len(ds))
		c.lock()
		port := c.LocalPort()
		id := c.config.UpnpID
		c.unlock()

		for _, d := range ds {
			if c, err := addPortMapping(d, upnp.TCP, port, id); err == nil {
				if !yield(c) {
					return
				}
			} else {
				log.Println("unable to map port", err)
			}

			if c, err := addPortMapping(d, upnp.UDP, port, id); err == nil {
				if !yield(c) {
					return
				}
			} else {
				log.Println("unable to map port", err)
			}
		}
	}, nil
}
