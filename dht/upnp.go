package dht

import (
	"context"
	"errors"
	"iter"
	"log"
	"net/netip"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/upnp"
)

func addPortMapping(d upnp.Device, proto upnp.Protocol, internalPort uint16, upnpID string, lease time.Duration) (_zero netip.AddrPort, err error) {
	ctx, done := context.WithTimeout(context.Background(), time.Minute)
	defer done()

	ip, err := d.GetExternalIPv4Address(ctx)
	if err != nil {
		return _zero, errorsx.Wrapf(err, "error adding %s port mapping unable to determined external ip", proto)
	}

	for i := internalPort; ; i++ {
		var (
			derp upnp.ErrUPnP
		)

		externalPort, err := d.AddPortMapping(ctx, proto, int(internalPort), int(i), upnpID, lease)
		if err == nil {
			return netip.AddrPortFrom(netip.AddrFrom16([16]byte(ip.To16())), uint16(externalPort)), nil
		}

		if errors.As(err, &derp) && derp.Code == 718 { // conflict in port mapping
			continue
		}

		return _zero, errorsx.Wrapf(err, "error adding %s port mapping", proto)
	}
}

func UPnPPortForward(ctx context.Context, id string, port uint16) (iter.Seq[netip.AddrPort], error) {
	id = langx.FirstNonZero(id, errorsx.Must(uuid.NewV7()).String())
	return func(yield func(netip.AddrPort) bool) {
		const (
			lease = time.Hour
		)
		for {
			ds := upnp.Discover(ctx, 0, 2*time.Second)

			for _, d := range ds {
				if c, err := addPortMapping(d, upnp.TCP, port, id, lease); err == nil {
					if !yield(c) {
						return
					}
				} else {
					log.Println("unable to map port", err)
				}

				if c, err := addPortMapping(d, upnp.UDP, port, id, lease); err == nil {
					if !yield(c) {
						return
					}
				} else {
					log.Println("unable to map port", err)
				}
			}

			select {
			case <-time.After(lease):
			case <-ctx.Done():
				log.Println("context done upnp shutting down", ctx.Err())
				return
			}
		}
	}, nil
}
