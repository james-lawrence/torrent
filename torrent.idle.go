package torrent

import (
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
)

// touch records that the torrent made progress. only call this at low frequency
// events (connection changes, validated pieces), never per chunk.
func (t *torrent) touch() {
	t.lastActivity.Store(new(time.Now()))
}

// idle reports whether the torrent has been inactive for longer than its idle timeout.
// when not idle, wait is how long until the torrent could first become idle.
func (t *torrent) idle() (idle bool, wait time.Duration) {
	stats := t.Stats()

	// a seeder with live connections is never idle.
	if stats.Seeding && stats.ActivePeers > 0 {
		return false, t.idleTimeout
	}

	if remaining := t.idleTimeout - time.Since(stats.LastActivity); remaining > 0 {
		return false, remaining
	}

	return true, 0
}

// TuneIdleAutoUnload unloads the torrent from memory once it has been idle for the torrent's
// idle timeout. see TuneIdleBehavior.
func TuneIdleAutoUnload(t *torrent) error {
	return TuneIdleBehavior(func(t *torrent) {
		t.cln.config.debug().Println("unloading idle torrent", t.md.ID)
		errorsx.Log(errorsx.Wrapf(t.cln.Stop(t.Metadata()), "failed to unload idle torrent: %s", t.md.ID))
	})(t)
}

// TuneIdleBehavior spawns a goroutine that calls fn once the torrent has been idle for the
// torrent's idle timeout, then exits. does nothing when the idle timeout is disabled.
// the timeout is cloned from the client config, each application spawns its own watcher.
func TuneIdleBehavior(fn func(t *torrent)) Tuner {
	return func(t *torrent) error {
		if t.idleTimeout <= 0 {
			return nil
		}

		go func() {
			wait := t.idleTimeout

			for {
				select {
				case <-t.closed:
					return
				case <-time.After(wait):
				}

				var idle bool
				if idle, wait = t.idle(); !idle {
					continue
				}

				fn(t)
				return
			}
		}()

		return nil
	}
}
