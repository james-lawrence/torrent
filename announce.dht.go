package torrent

import (
	"context"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/errorsx"
)

func DHTAnnounceOnce(ctx context.Context, cln *Client, d *dht.Server, id int160.T) (err error) {
	// log.Println("dht announced initiated", id, cln.LocalPort16())
	// defer log.Println("dht announced completed", id, cln.LocalPort16())
	announced, err := d.AnnounceTraversal(ctx, id.AsByteArray(), dht.AnnouncePeer(true, cln.LocalPort()))
	if err != nil {
		return errorsx.Wrapf(err, "dht failed to announce: %s", id)
	}
	defer announced.Close()

	for {
		// log.Println("announce dht pending", id)
		select {
		// case pv := <-announced.Peers:
		// log.Println("announce dht peers", id, len(pv.Peers), pv.Peers)
		case <-announced.Peers:
			continue
		case <-announced.Finished():
			// log.Println("announce dht finished", id)
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}
