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
)

func AutoDetectIP(ctx context.Context, q *Server, id int160.T, local net.PacketConn, port uint16) (iter.Seq[netip.AddrPort], error) {
	return func(yield func(netip.AddrPort) bool) {
		const lease = time.Hour

		for {
			detect := traversal2.New(int160.Random(), NewTraversalQuerier(q), traversal2.WithSeeds(q.ClosestGoodNodeInfos(8, id)...))

			for v := range detect.Results(ctx) {
				log.Println("DERP DERP DERP", netip.AddrPortFrom(v.IP.Addr(), port))
				if !yield(netip.AddrPortFrom(v.IP.Addr(), port)) {
					return
				}

				break
			}

			if err := detect.Err(); err != nil {
				log.Println("auto detect port failed", err)
			}

			time.Sleep(lease)
		}
	}, nil
}
