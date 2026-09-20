package torrent

import (
	"context"
	"errors"
	"iter"
	"log"
	"net"
	"net/url"
	"runtime/trace"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/backoffx"
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
			// a task per tracker, it ends before the result is yielded so it only measures the announce.
			tctx, task := trace.NewTask(ctx, "torrent.announce.tracker")
			trace.Log(tctx, "tracker.uri", traceTrackerURI(uri))
			trace.Logf(tctx, "tracker.initiated", "infohash=%s trackers=%d metadata=%t", t.md.ID, len(ts), t.info != nil)
			actx, done := context.WithTimeout(tctx, time.Minute)
			d, peers, err := TrackerAnnounceOnce(actx, t, uri, options...)
			done()
			trace.Logf(tctx, "tracker.result", "outcome=%s peers=%d interval=%s", traceTrackerOutcome(err), len(peers), d)
			task.End()

			failed := errorsx.Ignore(err, ErrNoPeers, context.DeadlineExceeded)

			if !yield(trackerresponse{Peers: peers, Next: d, Err: failed}) {
				return
			}

			// the task has ended, these attach to the caller's task.
			if failed != nil {
				trace.Logf(ctx, "tracker.failed", "infohash=%s tracker=%s outcome=%s metadata=%t", t.md.ID, traceTrackerURI(uri), traceTrackerOutcome(failed), t.info != nil)
				continue
			}

			trace.Logf(ctx, "tracker.completed", "infohash=%s tracker=%s trackers=%d", t.md.ID, traceTrackerURI(uri), len(ts))
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

	// reading the stats resets the transfer counters, so it is its own region to
	// correlate with the announce that carries them.
	region := trace.StartRegion(ctx, "tracker.event.stats")
	err = l.Tune(
		TuneResetTrackingStats(&s),
		TuneReadPeerID(&id),
		TuneReadHashID(&infoid),
		TuneReadAnnounce(&announcer),
		TuneReadPort(&port),
		TuneReadBytesRemaining(&remaining),
	)
	region.End()
	if err != nil {
		return nil, err
	}

	req := tracker.NewAccounceRequest(
		id,
		port,
		infoid,
		tracker.AnnounceOptionKey,
		tracker.AnnounceOptionDownloaded(s.BytesValidated.Int64()),
		tracker.AnnounceOptionUploaded(s.BytesWrittenData.n),
		tracker.AnnounceOptionRemaining(remaining),
		langx.Compose(options...),
	)

	// the peer id and key are deliberately not logged, trackers correlate on them.
	trace.Logf(ctx, "tracker.request", "event=%s uploaded=%d downloaded=%d left=%d port=%d", req.Event, req.Uploaded, req.Downloaded, req.Left, req.Port)

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
		delay      = mindelay
		forcecheck = time.Now().Add(maxdelay)
	)

	if len(t.md.Trackers) == 0 {
		log.Println(t.md.ID, "ignoring announce, trackers not available")
		return
	}

	// a task for the lifetime of the loop, it attributes the events between rounds (skips, sleeps,
	// completion) to a torrent. the ctx is replaced so everything below nests under it. it is only
	// recorded when the loop starts during a trace capture, otherwise the events still carry its id
	// but the task itself is absent from the trace, so the infohash is repeated on each round.
	ctx, loop := trace.NewTask(ctx, "torrent.announce.loop")
	defer loop.End()
	trace.Logf(ctx, "torrent.infohash", "%s", t.md.ID.String())
	trace.Logf(ctx, "torrent.trackers", "%d", len(t.md.Trackers))

	for {
		var (
			totalpeers       = 0
			failed     error = nil
		)

		if ts := time.Now(); !t.wantPeers() && ts.Before(forcecheck) {
			trace.Logf(ctx, "announce.skipped", "infohash=%s peers not wanted, sleep=%s next force=%s", t.md.ID.String(), mindelay, forcecheck.Format(time.RFC3339))
			time.Sleep(mindelay)
			continue
		} else {
			forcecheck = ts.Add(maxdelay + backoffx.Random(mindelay))
		}

		trackers := trackerseq(t.md.Trackers)

		// a task per round nested under the loop, it measures the announces of a single round.
		rctx, task := trace.NewTask(ctx, "torrent.announce.round")
		trace.Logf(rctx, "torrent.infohash", "%s", t.md.ID.String())
		trace.Logf(rctx, "torrent.trackers", "%d", len(trackers))

		for res := range trackers.Peers(rctx, t, options...) {
			totalpeers += len(res.Peers)

			if res.Err == nil {
				failed = nil
				t.addPeers(res.Peers...)
				continue
			}

			failed = langx.FirstNonNil(failed, res.Err)
			delay = max(delay, res.Next)
		}

		task.End()

		if errorsx.Is(failed, tracker.ErrMissingInfoHash) && len(trackers) == 1 {
			trace.Log(ctx, "announce.stop", "missing infohash with a single tracker")
			t.cln.config.errors().Println(errorsx.Wrap(failed, "hard stop due to no infohash and a single tacker"))
			t.cln.Stop(t.Metadata())
			return
		}

		if totalpeers == 0 && failed == nil {
			trace.Log(ctx, "announce.nopeers", "succeeded without peers, retrying without delay")
			continue
		}

		trace.Logf(ctx, "announce.delay", "sleep=%s peers=%d outcome=%s", delay, totalpeers, traceTrackerOutcome(failed))
		region := trace.StartRegion(ctx, "announce.sleep")
		time.Sleep(delay)
		region.End()
		delay = mindelay

		if donefn() {
			trace.Log(ctx, "announce.done", "completion condition satisfied")
			return
		}
	}
}

// traceTrackerURI reduces a tracker url to scheme://host. private trackers embed passkeys
// in the path or query and a trace is an artifact that gets shared, so this is the only
// form of a tracker url that may be emitted into a trace.
func traceTrackerURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		// the parse error embeds the url.
		return "invalid"
	}

	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// traceTrackerOutcome classifies the result of an announce without emitting the error text.
// announce errors embed the tracker url (TrackerEvent wraps them with it, net/http embeds the
// request url) and trackers can echo user specific details in their failure reasons.
func traceTrackerOutcome(err error) string {
	var (
		uerr *url.Error
		nerr net.Error
	)

	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNoPeers):
		return "no peers"
	case errors.Is(err, tracker.ErrMissingInfoHash):
		return "missing infohash"
	case errors.Is(err, tracker.ErrBadScheme):
		return "unsupported scheme"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &uerr):
		return uerr.Err.Error()
	case errors.As(err, &nerr):
		return nerr.Error()
	default:
		return "rejected"
	}
}
