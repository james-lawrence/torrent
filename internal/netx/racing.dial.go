package netx

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/internal/asynccompute"
	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
)

// Creates a pool of go routines for dialing addresses allowing for dialing over
// a set of different networks and picking the fastest one.
func NewRacing(n uint16) *RacingDialer {
	return &RacingDialer{
		arena: asynccompute.New(func(ctx context.Context, w racingdialworkload) error {
			dctx, done := context.WithTimeout(ctx, w.timeout)
			defer done()

			c, err := w.network.Dial(dctx, w.address)
			if err == nil {
				select {
				case <-ctx.Done():
					errorsx.Log(c.Close())
				case w.fastest <- c:
					w.done(nil)
				}

				if w.outstanding.Add(^uint32(0)) == 0 {
					w.done(err)
					close(w.fastest)
				}

				return nil
			}

			w.failure.CompareAndSwap(nil, langx.Autoptr(err))
			if w.outstanding.Add(^uint32(0)) == 0 {
				w.done(err)
				close(w.fastest)
			}

			return nil
		}, asynccompute.Backlog[racingdialworkload](n)),
	}
}

type DialableNetwork interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

func initRacingDial(address string, d time.Duration, n uint64, done context.CancelCauseFunc) racingdialworkload {
	return racingdialworkload{
		address:     address,
		timeout:     d,
		fastest:     make(chan net.Conn, 1),
		failure:     atomicx.Pointer[error](nil),
		done:        done,
		outstanding: atomicx.Uint32(n),
	}
}

type racingdialworkload struct {
	network     DialableNetwork
	address     string
	timeout     time.Duration
	done        context.CancelCauseFunc
	failure     *atomic.Pointer[error]
	fastest     chan net.Conn
	outstanding *atomic.Uint32
}

type RacingDialer struct {
	arena *asynccompute.Pool[racingdialworkload]
}

func (t RacingDialer) Dial(ctx context.Context, timeout time.Duration, address string, networks ...DialableNetwork) (net.Conn, error) {
	if len(networks) == 0 {
		return nil, errorsx.String("no networks to dial")
	}

	_ctx, done := context.WithTimeout(ctx, timeout)
	defer done()
	__ctx, cancel := context.WithCancelCause(_ctx)
	defer cancel(nil)

	w := initRacingDial(address, timeout, uint64(len(networks)), cancel)

	for _, n := range networks {
		dup := w
		dup.network = n

		if err := t.arena.Run(__ctx, dup); err != nil {
			cancel(errorsx.Wrapf(err, "timeout: %d", timeout))
			return nil, err
		}
	}

	var fastest net.Conn

	select {
	case <-_ctx.Done():
	case fastest = <-w.fastest:
		cancel(nil)
	}

	failure := errorsx.Compact(langx.Zero(w.failure.Load()), context.Cause(__ctx), context.Cause(_ctx))

	if fastest == nil {
		return nil, failure
	}

	return fastest, errorsx.Ignore(failure, context.Canceled)
}
