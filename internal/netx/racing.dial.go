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
				w.outstanding.Add(^uint32(0))

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

func (t RacingDialer) Dial(_ctx context.Context, timeout time.Duration, address string, networks ...DialableNetwork) (net.Conn, error) {
	_ctx, done := context.WithTimeout(_ctx, timeout)
	defer done()
	_ctx, cancel := context.WithCancelCause(_ctx)
	defer cancel(nil)

	w := initRacingDial(address, timeout, uint64(len(networks)), cancel)

	queue := func(__ctx context.Context, _w racingdialworkload) error {
		return t.arena.Run(__ctx, _w)
	}

	for _, n := range networks {
		dup := w
		dup.network = n

		if err := queue(_ctx, dup); err != nil {
			cancel(err)
			return nil, err
		}
	}

	var fastest net.Conn

	select {
	case <-_ctx.Done():
	case fastest = <-w.fastest:
	}

	failure := langx.Autoderef(w.failure.Load())

	if fastest == nil {
		return nil, errorsx.Compact(failure, context.Cause(_ctx))
	}

	failure = errorsx.Compact(failure, context.Cause(_ctx))
	return fastest, errorsx.Ignore(failure, context.Canceled)
}
