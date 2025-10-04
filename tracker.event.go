package torrent

import (
	"context"
	"log"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"

	"github.com/james-lawrence/torrent/tracker"
)

const ErrNoPeers = errorsx.String("failed to locate any peers for torrent")

func TrackerEvent(ctx context.Context, l Torrent, announceuri string, options ...tracker.AnnounceOption) (ret *tracker.AnnounceResponse, err error) {
	var (
		announcer tracker.Announce
		port      int
		s         Stats
		id        int160.T
		infoid    int160.T
		remaining int64
	)

	if err = l.Tune(
		TuneResetTrackingStats(&s),
		TuneReadPeerID(&id),
		TuneReadHashID(&infoid),
		TuneReadAnnounce(&announcer),
		TuneReadPort(&port),
		TuneReadBytesRemaining(&remaining),
	); err != nil {
		return nil, err
	}

	req := tracker.NewAccounceRequest(
		id,
		port,
		infoid,
		tracker.AnnounceOptionKey,
		tracker.AnnounceOptionDownloaded(s.BytesReadUsefulData.Int64()),
		tracker.AnnounceOptionUploaded(s.BytesWrittenData.n),
		tracker.AnnounceOptionRemaining(remaining),
		langx.Compose(options...),
	)

	res, err := announcer.ForTracker(announceuri).Do(ctx, req)
	return &res, errorsx.Wrapf(err, "announce: %s", announceuri)
}

func TrackerAnnounceOnce(ctx context.Context, l Torrent, uri string, options ...tracker.AnnounceOption) (delay time.Duration, peers Peers, err error) {
	ctx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()

	announced, err := TrackerEvent(ctx, l, uri, options...)
	if err != nil {
		return delay, nil, err
	}

	if d := time.Duration(announced.Interval) * time.Second; delay < d {
		delay = d
	}

	if len(announced.Peers) == 0 {
		return delay, nil, ErrNoPeers
	}

	return delay, peers.AppendFromTracker(announced.Peers), nil
}

func TrackerAnnounceUntil(ctx context.Context, t *torrent, donefn func() bool, options ...tracker.AnnounceOption) {
	const mindelay = 1 * time.Minute
	var delay time.Duration = mindelay

	trackers := t.md.Trackers

	for {
		for _, uri := range trackers {
			t.cln.config.debug().Println("announced initiated", t.md.DisplayName, t.Metadata().DisplayName, len(trackers), uri)
			ctx, done := context.WithTimeout(context.Background(), time.Minute)
			d, peers, err := TrackerAnnounceOnce(ctx, t, uri, options...)
			done()
			t.cln.config.debug().Println("announced completed", t.md.DisplayName, t.Metadata().DisplayName, len(trackers), uri)

			if errorsx.Is(err, context.DeadlineExceeded) {
				t.cln.config.errors().Println(err)
				continue
			}

			if errorsx.Is(err, tracker.ErrMissingInfoHash) {
				t.cln.config.errors().Println(err)
				if len(trackers) == 1 {
					return
				}

				continue
			}

			if err == nil {
				t.addPeers(peers...)
				go t.maybeNewConns()
				continue
			}

			if delay < d {
				delay = d
			}

			if err == ErrNoPeers {
				log.Println("announce succeeded, but there are no peers")
				continue
			}

			t.cln.config.debug().Println("announce failed", t.info == nil, err)
		}

		t.cln.config.debug().Println("announce sleeping for maximum delay", t.Metadata().ID.String(), delay)
		time.Sleep(delay)
		delay = mindelay

		if donefn() {
			// log.Println("announce completed", t.Metadata().ID.String())
			return
		}
	}
}
