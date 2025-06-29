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
// note: any tcp connections get set linger(0)
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
				atomic.AddUint64(w.outstanding, ^uint64(0))

				return nil
			}

			w.failure.CompareAndSwap(nil, langx.Autoptr(err))
			if atomic.AddUint64(w.outstanding, ^uint64(0)) == 0 {
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
		outstanding: langx.Autoptr(n),
	}
}

type racingdialworkload struct {
	network     DialableNetwork
	address     string
	timeout     time.Duration
	done        context.CancelCauseFunc
	failure     *atomic.Pointer[error]
	fastest     chan net.Conn
	outstanding *uint64
}

type RacingDialer struct {
	arena *asynccompute.Pool[racingdialworkload]
}

func (t RacingDialer) Dial(ctx context.Context, timeout time.Duration, address string, networks ...DialableNetwork) (net.Conn, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	w := initRacingDial(address, timeout, uint64(len(networks)), cancel)

	for _, n := range networks {
		dup := w
		dup.network = n

		if err := t.arena.Run(ctx, dup); err != nil {
			cancel(err)
			return nil, err
		}
	}

	var fastest net.Conn

	select {
	case <-ctx.Done():
	case fastest = <-w.fastest:
	}

	failure := langx.Autoderef(w.failure.Load())

	if fastest == nil {
		return nil, errorsx.Compact(failure, context.Cause(ctx))
	}

	failure = errorsx.Compact(failure, context.Cause(ctx))
	return fastest, errorsx.Ignore(failure, context.Canceled)
}
