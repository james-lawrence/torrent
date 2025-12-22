package torrent

import (
	"context"
	"iter"
	"log"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"

	"github.com/james-lawrence/torrent/tracker"
)

const ErrNoPeers = errorsx.String("failed to locate any peers for torrent")

type trackerresponse struct {
	Peers
	Err  error
	Next time.Duration
}
type trackerseq []string

func (ts trackerseq) Peers(ctx context.Context, t *torrent, options ...tracker.AnnounceOption) iter.Seq[trackerresponse] {
	return func(yield func(trackerresponse) bool) {
		for _, uri := range ts {
			t.cln.config.debug().Println("announced initiated", t.md.DisplayName, t.Metadata().DisplayName, len(ts), uri)
			ctx, done := context.WithTimeout(ctx, time.Minute)
			d, peers, err := TrackerAnnounceOnce(ctx, t, uri, options...)
			done()

			failed := errorsx.Ignore(err, ErrNoPeers, context.DeadlineExceeded)

			if !yield(trackerresponse{Peers: peers, Next: d, Err: failed}) {
				return
			}

			if failed != nil {
				t.cln.config.errors().Println("announce failed", t.info == nil, failed)
				continue
			}

			t.cln.config.debug().Println("announced completed", t.md.DisplayName, t.Metadata().DisplayName, len(ts), uri)
		}
	}
}

func TrackerEvent(ctx context.Context, l Torrent, announceuri string, options ...tracker.AnnounceOption) (ret *tracker.AnnounceResponse, err error) {
	var (
		announcer tracker.Announce
		port      uint16
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

	delay = max(delay, time.Duration(announced.Interval)*time.Second)

	if len(announced.Peers) == 0 {
		return delay, nil, ErrNoPeers
	}

	return delay, peers.AppendFromTracker(announced.Peers), nil
}

func TrackerAnnounceUntil(ctx context.Context, t *torrent, donefn func() bool, options ...tracker.AnnounceOption) {
	const (
		maxdelay = 1 * time.Hour
		mindelay = 1 * time.Minute
	)

	var (
		delay     = mindelay
		lastcheck = time.Now()
	)

	trackers := trackerseq(t.md.Trackers)

	for {
		var (
			totalpeers       = 0
			failed     error = nil
		)

		if ts, vs := time.Now(), lastcheck.Add(maxdelay); !t.wantPeers() && ts.Before(vs) {
			log.Println(t.md.ID, "skipping announce, peers not wanted", mindelay, "next foce", vs)
			time.Sleep(mindelay)
			continue
		} else {
			lastcheck = ts
		}

		for res := range trackers.Peers(ctx, t, options...) {
			totalpeers += len(res.Peers)

			if res.Err == nil {
				failed = nil
				t.addPeers(res.Peers...)
				continue
			}

			failed = langx.FirstNonNil(failed, res.Err)
			delay = max(delay, res.Next)
		}

		if errorsx.Is(failed, tracker.ErrMissingInfoHash) && len(trackers) == 1 {
			t.cln.config.errors().Println(errorsx.Wrap(failed, "hard stop due to no infohash and a single tacker"))
			t.cln.Stop(t.Metadata())
			return
		}

		if totalpeers > 0 && failed == nil {
			log.Println("announce succeeded, but there are no peers", t.Metadata().ID.String(), len(trackers))
			continue
		}

		t.cln.config.debug().Println("announce sleeping for maximum delay", t.Metadata().ID.String(), delay)
		time.Sleep(delay)
		delay = mindelay

		if donefn() {
			return
		}
	}
}
