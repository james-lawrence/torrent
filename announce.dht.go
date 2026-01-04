package torrent

import (
	"context"
	"iter"
	"log"

	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/iterx"
)

func DHTAnnounceOnce(ctx context.Context, cln *Client, d *dht.Server, id int160.T) (err error) {
	// log.Println("dht announced initiated", id, cln.LocalPort16())
	// defer log.Println("dht announced completed", id, cln.LocalPort16())
	announced, err := d.AnnounceTraversal(ctx, id, dht.AnnouncePeer(true, cln.LocalPort16()))
	if err != nil {
		return errorsx.Wrapf(err, "dht failed to announce: %s", id)
	}
	defer announced.Close()

	for {
		select {
		case pv := <-announced.Peers:
			log.Println("announce dht peers", id, len(pv.Peers), pv.Peers)
			continue
		case <-announced.Finished():
			log.Println("announce dht finished", id)
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

type announceseq struct {
	announced *dht.Announce
	failed    error
}

func (t announceseq) Each(ctx context.Context) iter.Seq[dht.PeersValues] {
	return func(yield func(dht.PeersValues) bool) {
		defer t.announced.Close()
		for {
			select {
			case pv := <-t.announced.Peers:
				if !yield(pv) {
					return
				}
			case <-t.announced.Finished():
				return
			case <-ctx.Done():
				t.failed = context.Cause(ctx)
				return
			}
		}
	}
}

func (t *announceseq) Err() error {
	return t.failed
}

func DHTAnnounce(ctx context.Context, d *dht.Server, id int160.T) (iterx.Seq[dht.PeersValues], error) {
	announced, err := d.AnnounceTraversal(ctx, id, dht.AnnouncePeer(false, d.AddrPort().Port()))
	if err != nil {
		return nil, errorsx.Wrapf(err, "dht failed to announce: %s", id)
	}

	return &announceseq{announced: announced}, nil
}
