package torrent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/james-lawrence/torrent/bep0019"
	"github.com/james-lawrence/torrent/internal/backoffx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/metainfo"
)

// WebseedStats is a snapshot of a single webseed's activity for the
// lifetime of its worker.
type WebseedStats struct {
	Root          string
	BytesFetched  int64
	ChunksFetched int64
	Errors        int64
	Started       time.Time
	Stopped       time.Time
}

func (s WebseedStats) String() string {
	return fmt.Sprintf(
		"webseed(%s) chunks(%d) bytes(%d) errors(%d) duration(%s)",
		s.Root, s.ChunksFetched, s.BytesFetched, s.Errors, s.Stopped.Sub(s.Started),
	)
}

// fileSpan is the byte range [start, end) a file occupies within the
// concatenated view of a torrent's data.
type fileSpan struct {
	file  metainfo.FileInfo
	start int64
	end   int64
}

func webseedFileSpans(info *metainfo.Info) []fileSpan {
	files := info.UpvertedFiles()
	spans := make([]fileSpan, 0, len(files))
	for _, f := range files {
		start := f.Offset(info)
		spans = append(spans, fileSpan{file: f, start: start, end: start + f.Length})
	}
	return spans
}

// webseedWorker fetches chunks for a single torrent from a single BEP19
// webseed url, in place of a BitTorrent peer connection.
type webseedWorker struct {
	t     *torrent
	root  string
	cl    *bep0019.Client
	files []fileSpan

	started       time.Time
	bytesFetched  atomic.Int64
	chunksFetched atomic.Int64
	errors        atomic.Int64
}

func newWebseedWorker(t *torrent, root string, cl *bep0019.Client) *webseedWorker {
	return &webseedWorker{
		t:       t,
		root:    root,
		cl:      cl,
		files:   webseedFileSpans(t.info),
		started: time.Now(),
	}
}

// Stats returns a snapshot of this worker's activity so far.
func (w *webseedWorker) Stats() WebseedStats {
	return WebseedStats{
		Root:          w.root,
		BytesFetched:  w.bytesFetched.Load(),
		ChunksFetched: w.chunksFetched.Load(),
		Errors:        w.errors.Load(),
		Started:       w.started,
		Stopped:       time.Now(),
	}
}

func (w *webseedWorker) run(ctx context.Context) {
	defer func() {
		w.t.cln.config.info().Println("webseed stopped", w.Stats())
	}()

	strategy := backoffx.New(
		backoffx.Exponential(time.Second),
		backoffx.Maximum(time.Minute),
		backoffx.Jitter(0.5),
	)
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.t.closed:
			return
		default:
		}

		err := w.step(ctx)

		var idle empty
		switch {
		case err == nil:
			attempt = 0
			continue
		case errors.As(err, &idle):
			// nothing outstanding to fetch right now; wait for more chunks
			// to be requested or the torrent to complete.
			attempt = 0
		default:
			w.errors.Add(1)
			w.t.cln.config.warn().Println(errorsx.Wrap(err, "webseed fetch failed"), w.root)
			attempt++
		}

		select {
		case <-ctx.Done():
			return
		case <-w.t.closed:
			return
		case <-time.After(strategy.Backoff(attempt)):
		}
	}
}

// step claims a batch of outstanding chunk requests (roughly a piece's
// worth) and fetches them from the webseed.
func (w *webseedWorker) step(ctx context.Context) error {
	t := w.t

	n := int(chunksPerPiece(t.info.PieceLength, t.chunks.clength))
	if n <= 0 {
		n = 1
	}

	available := t.chunks.fill(roaring.New(), uint64(t.chunks.cmaximum))
	reqs, err := t.chunks.Pop(n, available)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		return empty{}
	}

	groups := map[int][]request{}
	for _, r := range reqs {
		groups[int(r.Index)] = append(groups[int(r.Index)], r)
	}

	var ferr error
	for pid, group := range groups {
		if err := w.fetchPiece(ctx, pid, group); err != nil {
			ferr = err
			t.chunks.Retry(group...)
		}
	}

	return ferr
}

// fetchPiece retrieves the byte range covered by group (all belonging to
// piece pid) from the webseed, splitting the request at file boundaries as
// needed, and writes/verifies each chunk once the full range is retrieved.
func (w *webseedWorker) fetchPiece(ctx context.Context, pid int, group []request) error {
	t := w.t
	info := t.info
	pieceStart := int64(pid) * info.PieceLength

	lo, hi := int64(-1), int64(-1)
	for _, r := range group {
		b := pieceStart + int64(r.Begin)
		e := b + int64(r.Length)
		if lo == -1 || b < lo {
			lo = b
		}
		if hi == -1 || e > hi {
			hi = e
		}
	}

	full := make([]byte, hi-lo)
	for _, span := range w.files {
		if span.end <= lo || span.start >= hi {
			continue
		}

		segStart := max(lo, span.start)
		segEnd := min(hi, span.end)

		rawurl, err := bep0019.URL(w.root, info, span.file)
		if err != nil {
			return err
		}

		data, err := w.cl.Fetch(ctx, rawurl, segStart-span.start, segEnd-segStart)
		if err != nil {
			return err
		}

		copy(full[segStart-lo:segEnd-lo], data)
	}

	var n int64
	for _, r := range group {
		b := pieceStart + int64(r.Begin) - lo
		e := b + int64(r.Length)
		chunk := full[b:e]

		if err := t.writeChunk(int(r.Index), int64(r.Begin), chunk); err != nil {
			return errorsx.Wrap(err, "failed to write webseed chunk")
		}
		if err := t.chunks.Verify(r); err != nil {
			return errorsx.Wrap(err, "failed to verify webseed chunk")
		}
		if t.chunks.ChunksAvailable(uint64(r.Index)) {
			t.digests.Enqueue(uint64(r.Index))
		}
		n += int64(len(chunk))
	}

	t.stats.BytesReadUsefulData.Add(n)
	t.cln.stats.BytesReadUsefulData.Add(n)
	w.bytesFetched.Add(n)
	w.chunksFetched.Add(int64(len(group)))

	return nil
}

// webseedURLs returns the subset of Metadata.Webseeds this package supports:
// HTTP(S) BEP19 mirrors. FTP webseeds (BEP19 also permits ftp://) are not
// implemented.
func webseedURLs(md Metadata) []string {
	urls := make([]string, 0, len(md.Webseeds))
	for _, u := range md.Webseeds {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			urls = append(urls, u)
		}
	}
	return urls
}

func (t *torrent) startWebseeds() {
	urls := webseedURLs(t.md)
	if len(urls) == 0 || t.info == nil {
		return
	}

	t.webseedOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		t.webseedCancel = cancel

		cl := bep0019.NewClient(t.cln.config.dialer, 30*time.Second)

		workers := make([]*webseedWorker, 0, len(urls))
		for _, root := range urls {
			workers = append(workers, newWebseedWorker(t, root, cl))
		}

		t.webseedMu.Lock()
		t.webseeds = append(t.webseeds, workers...)
		t.webseedMu.Unlock()

		for _, w := range workers {
			go w.run(ctx)
		}
	})
}

func (t *torrent) stopWebseeds() {
	if t.webseedCancel != nil {
		t.webseedCancel()
	}
}

// WebseedStats returns a snapshot of each active webseed's activity.
func (t *torrent) WebseedStats() []WebseedStats {
	t.webseedMu.RLock()
	defer t.webseedMu.RUnlock()

	stats := make([]WebseedStats, 0, len(t.webseeds))
	for _, w := range t.webseeds {
		stats = append(stats, w.Stats())
	}
	return stats
}
