package dht

import (
	"context"
	"iter"
	"log"
	"net/netip"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/traversal2"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/netx"
)

func AutoDetectIP(ctx context.Context, sc *Server, b Binding, id int160.T, bestaddr netip.AddrPort, port uint16) (iter.Seq[netip.AddrPort], error) {
	return func(yield func(netip.AddrPort) bool) {
		const (
			minK       = 8
			shortretry = time.Minute
			lease      = time.Hour
		)

		b := sc.Binding(bestaddr)

		collect := func() (map[netip.AddrPort]struct{}, error) {
			_ctx, done := context.WithTimeout(ctx, time.Minute)
			defer done()

			detect := traversal2.New(
				int160.Random(),
				NewTraversalQuerier(b.ID(), sc),
				traversal2.WithSeeds(sc.ClosestGoodNodeInfos(bestaddr, minK, id)...),
			)
			m := map[netip.AddrPort]struct{}{}
			for v := range detect.Results(_ctx) {
				addr := netip.AddrPortFrom(v.IP.Addr(), port)
				if !addr.IsValid() || !netx.Reachable(addr, bestaddr) {
					continue
				}

				if !netx.Reachable(addr, bestaddr) {
					log.Println(bestaddr, "ignoring unreachable node:", addr)
					continue
				}
				m[addr] = struct{}{}
			}

			return m, errorsx.Ignore(detect.Err(), context.DeadlineExceeded)
		}

		for {
			if gnodes := sc.numGoodNodes(); gnodes < minK {
				if !yield(b.AddrPort()) {
					return
				}

				time.Sleep(time.Second)
				continue
			}

			m, err := collect()

			for addr := range m {
				if !yield(addr) {
					return
				}
			}

			if err != nil {
				log.Println("auto detect port failed", err)
			}

			if len(m) == 0 {
				time.Sleep(shortretry)
				continue
			}

			time.Sleep(lease)
		}
	}, nil
}
