package dht

import (
	"context"
	"iter"
	"log"
	"net"
	"net/netip"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/traversal2"
	"github.com/james-lawrence/torrent/internal/errorsx"
)

func AutoDetectIP(ctx context.Context, q *Server, id int160.T, local net.PacketConn, port uint16) (iter.Seq[netip.AddrPort], error) {
	return func(yield func(netip.AddrPort) bool) {
		const (
			minK       = 8
			shortretry = time.Minute
			lease      = time.Hour
		)

		collect := func() (map[netip.AddrPort]struct{}, error) {
			_ctx, done := context.WithTimeout(ctx, time.Minute)
			defer done()

			detect := traversal2.New(int160.Random(), NewTraversalQuerier(q), traversal2.WithSeeds(q.ClosestGoodNodeInfos(minK, id)...))
			m := map[netip.AddrPort]struct{}{}
			for v := range detect.Results(_ctx) {
				addr := netip.AddrPortFrom(v.IP.Addr(), port)
				if !addr.IsValid() {
					continue
				}

				m[addr] = struct{}{}
			}

			return m, errorsx.Ignore(detect.Err(), context.DeadlineExceeded)
		}

		for {
			if gnodes := q.numGoodNodes(); gnodes < minK {
				if !yield(*q.dynamicaddr.Load()) {
					return
				}

				time.Sleep(200 * time.Millisecond)
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
