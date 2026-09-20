package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/trace"
	"sync/atomic"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/testutil"
	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/tracker"
	"github.com/stretchr/testify/require"
)

func TestTraceTrackerURI(t *testing.T) {
	t.Run("strips passkey from the path", func(t *testing.T) {
		require.Equal(t, "https://tracker.example.com:8443", traceTrackerURI("https://tracker.example.com:8443/PASSKEY123/announce"))
	})

	t.Run("strips passkey from the query", func(t *testing.T) {
		require.Equal(t, "http://tracker.example.com", traceTrackerURI("http://tracker.example.com/announce?passkey=PASSKEY123&extra=1"))
	})

	t.Run("strips userinfo", func(t *testing.T) {
		require.Equal(t, "udp://tracker.example.com:6969", traceTrackerURI("udp://user:PASSKEY123@tracker.example.com:6969/announce"))
	})

	t.Run("does not echo an unparseable url", func(t *testing.T) {
		// the control character makes url.Parse fail, and its error embeds the input.
		require.Equal(t, "invalid", traceTrackerURI("http://tracker.example.com/PASSKEY123\x7f/announce"))
	})
}

func TestTraceTrackerOutcome(t *testing.T) {
	t.Run("classifies results", func(t *testing.T) {
		require.Equal(t, "ok", traceTrackerOutcome(nil))
		require.Equal(t, "no peers", traceTrackerOutcome(ErrNoPeers))
		require.Equal(t, "missing infohash", traceTrackerOutcome(errorsx.Wrapf(tracker.ErrMissingInfoHash, "announce: %s", "http://tracker.example.com/PASSKEY123/announce")))
		require.Equal(t, "unsupported scheme", traceTrackerOutcome(tracker.ErrBadScheme))
		require.Equal(t, "deadline exceeded", traceTrackerOutcome(fmt.Errorf("announce: %w", context.DeadlineExceeded)))
		require.Equal(t, "canceled", traceTrackerOutcome(fmt.Errorf("announce: %w", context.Canceled)))
	})

	t.Run("drops the request url that net/http and TrackerEvent embed in errors", func(t *testing.T) {
		err := errorsx.Wrapf(&url.Error{
			Op:  "Get",
			URL: "http://tracker.example.com/PASSKEY123/announce?info_hash=abc",
			Err: errors.New("dial tcp 192.0.2.1:80: connect: connection refused"),
		}, "announce: %s", "http://tracker.example.com/PASSKEY123/announce")

		outcome := traceTrackerOutcome(err)
		require.Equal(t, "dial tcp 192.0.2.1:80: connect: connection refused", outcome)
		require.NotContains(t, outcome, "PASSKEY123")
	})

	t.Run("never emits tracker controlled text", func(t *testing.T) {
		outcome := traceTrackerOutcome(fmt.Errorf("tracker gave failure reason: %q", "invalid passkey PASSKEY123"))
		require.Equal(t, "rejected", outcome)
		require.NotContains(t, outcome, "PASSKEY123")
	})
}

// runtime/trace is process global, the capture is inspected for strings since event
// and task names live in the raw trace's string table.
func TestTrackerAnnounceUntil(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()

	var capture bytes.Buffer
	require.NoError(t, trace.Start(&capture))
	stopped := false
	defer func() {
		if !stopped {
			trace.Stop()
		}
	}()

	t.Run("returns immediately when the torrent has no trackers", func(t *testing.T) {
		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
		)
		require.NoError(t, err)
		tor := newTorrent(cl, md)

		var donecalls atomic.Int32
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			TrackerAnnounceUntil(ctx, tor, func() bool { donecalls.Add(1); return true })
		}()

		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the loop did not return, it should have nothing to announce to")
		}

		require.Zero(t, donecalls.Load())
	})

	t.Run("hard stop on a missing infohash with a single tracker", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Write([]byte("d14:failure reason19:InfoHash not found.e"))
		}))
		defer srv.Close()

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
			OptionTrackers(srv.URL+"/PASSKEY123/announce"),
		)
		require.NoError(t, err)

		// inserted into the client so the hard stop has a torrent to drop.
		tor, err := cl.torrents.Insert(md, cl.newTorrent, tuneMerge(md))
		require.NoError(t, err)

		var donecalls atomic.Int32
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			TrackerAnnounceUntil(ctx, tor, func() bool { donecalls.Add(1); return true }, tracker.AnnounceOptionEventStarted)
		}()

		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the loop did not stop, it should hard stop when the only tracker reports a missing infohash")
		}

		require.EqualValues(t, 1, hits.Load())
		require.Zero(t, donecalls.Load(), "a hard stop must not wait for the completion condition")

		select {
		case <-tor.closed:
		default:
			t.Fatal("the hard stop should have stopped the torrent")
		}
	})

	t.Run("announces again without sleeping when a tracker succeeds without peers", func(t *testing.T) {
		// the first two announces succeed without peers, the third ends the loop.
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) <= 2 {
				w.Write([]byte("d8:completei0e10:incompletei0e8:intervali1800e5:peers0:e"))
				return
			}

			w.Write([]byte("d14:failure reason19:InfoHash not found.e"))
		}))
		defer srv.Close()

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
			OptionTrackers(srv.URL+"/PASSKEY123/announce"),
		)
		require.NoError(t, err)
		tor := newTorrent(cl, md)

		var donecalls atomic.Int32
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			TrackerAnnounceUntil(ctx, tor, func() bool { donecalls.Add(1); return true })
		}()

		// a sleep between the announces would be at least a minute.
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the loop slept between announces, it should retry immediately when there are no peers")
		}

		require.EqualValues(t, 3, hits.Load())
		require.Zero(t, donecalls.Load())
	})

	trace.Stop()
	stopped = true

	require.NotZero(t, capture.Len())
	for _, expected := range []string{
		"torrent.announce.loop",
		"torrent.announce.round",
		"torrent.infohash",
		"torrent.trackers",
		"torrent.announce.tracker",
		"tracker.event.stats",
		"tracker.request",
		"tracker.uri",
		"tracker.initiated",
		"tracker.result",
		"tracker.failed",
		"tracker.completed",
		"missing infohash",
		"announce.stop",
		"announce.nopeers",
	} {
		// not require.Contains, a failure would print the binary capture.
		require.True(t, bytes.Contains(capture.Bytes(), []byte(expected)), "missing from trace: %s", expected)
	}

	require.False(t, bytes.Contains(capture.Bytes(), []byte("PASSKEY123")), "a passkey reached the trace")
}

func TestTrackerseqPeers(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()

	var capture bytes.Buffer
	require.NoError(t, trace.Start(&capture))
	stopped := false
	defer func() {
		if !stopped {
			trace.Stop()
		}
	}()

	t.Run("a task per tracker records the outcome of each without leaking the url", func(t *testing.T) {
		healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("d8:completei1e10:incompletei2e8:intervali1800e5:peers6:\x01\x02\x03\x04\x00\x05e"))
		}))
		defer healthy.Close()

		// an address that was free a moment ago, nothing is listening on it.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		refused := l.Addr().String()
		require.NoError(t, l.Close())

		dir := t.TempDir()
		mi := testutil.GreetingTestTorrent(dir)

		cl, err := NewClient(TestingConfig(t, dir))
		require.NoError(t, err)
		defer cl.Close()

		md, err := New(
			mi.HashInfoBytes(),
			OptionStorage(storage.NewFile(t.TempDir())),
			OptionChunk(2),
			OptionInfo(mi.InfoBytes),
		)
		require.NoError(t, err)
		tor := newTorrent(cl, md)

		var results []trackerresponse
		for res := range (trackerseq{
			healthy.URL + "/PASSKEY123/announce",
			"http://" + refused + "/PASSKEY123/announce",
		}).Peers(ctx, tor) {
			results = append(results, res)
		}

		require.Len(t, results, 2)
		require.NoError(t, results[0].Err)
		require.Len(t, results[0].Peers, 1)
		// sanity check, the returned error is the leak vector the trace must not copy.
		require.ErrorContains(t, results[1].Err, "PASSKEY123")
	})

	trace.Stop()
	stopped = true

	require.NotZero(t, capture.Len())
	for _, expected := range []string{
		"torrent.announce.tracker",
		"tracker.uri",
		"tracker.initiated",
		"tracker.result",
		"tracker.completed",
		"tracker.failed",
		"outcome=ok peers=1",
		"connection refused",
	} {
		// not require.Contains, a failure would print the binary capture.
		require.True(t, bytes.Contains(capture.Bytes(), []byte(expected)), "missing from trace: %s", expected)
	}

	require.False(t, bytes.Contains(capture.Bytes(), []byte("PASSKEY123")), "a passkey reached the trace")
}
