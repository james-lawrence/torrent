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

type discoverer func(ctx context.Context) []upnp.Device

func UPnPPortForward(ctx context.Context, id string, port uint16, fallback netip.AddrPort) (iter.Seq[netip.AddrPort], error) {
	discover := func(ctx context.Context) []upnp.Device {
		return upnp.Discover(ctx, 0, 2*time.Second)
	}
	return upnpPortForward(ctx, id, port, fallback, discover, time.Hour)
}

func upnpPortForward(ctx context.Context, id string, port uint16, fallback netip.AddrPort, discover discoverer, lease time.Duration) (iter.Seq[netip.AddrPort], error) {
	id = langx.FirstNonZero(id, errorsx.Must(uuid.NewV7()).String())
	return func(yield func(netip.AddrPort) bool) {
		for {
			ds := discover(ctx)
			mapped := false

			for _, d := range ds {
				if c, err := addPortMapping(d, upnp.TCP, port, id, lease); err == nil {
					mapped = true
					if !yield(c) {
						return
					}
				} else {
					log.Println("upnp unable to map tcp port:", err)
				}

				if c, err := addPortMapping(d, upnp.UDP, port, id, lease); err == nil {
					mapped = true
					if !yield(c) {
						return
					}
				} else {
					log.Println("upnp unable to map udp port:", err)
				}
			}

			if !mapped {
				log.Println("upnp failed, using local address")
				if !yield(fallback) {
					return
				}
			}

			select {
			case <-time.After(lease):
			case <-ctx.Done():
				return
			}
		}
	}, nil
}
