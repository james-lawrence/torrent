package netx

import (
	"context"
	"net"
	"sync/atomic"

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
			// Try to avoid committing to a dial if the context is complete as it's difficult to determine
			// which dial errors allow us to forget the connection tracking entry handle.
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			c, err := w.network.Dial(ctx, w.address)
			if err != nil {
				w.failed.CompareAndSwap(nil, langx.Autoptr(err))
				return nil
			}

			// report we have liftoff.
			w.fastest.CompareAndSwap(nil, langx.Autoptr(c))
			w.done(nil)

			return nil
		}, asynccompute.Backlog[racingdialworkload](n)),
	}
}

type DialableNetwork interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

func initRacingDial(address string, done context.CancelCauseFunc) racingdialworkload {
	return racingdialworkload{
		address: address,
		fastest: atomicx.Pointer(net.Conn(nil)),
		failed:  atomicx.Pointer(error(nil)),
		done:    done,
	}
}

type racingdialworkload struct {
	network DialableNetwork
	address string
	done    context.CancelCauseFunc
	failed  *atomic.Pointer[error]
	fastest *atomic.Pointer[net.Conn]
}

type RacingDialer struct {
	arena *asynccompute.Pool[racingdialworkload]
}

func (t RacingDialer) Dial(ctx context.Context, address string, networks ...DialableNetwork) (net.Conn, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	w := initRacingDial(address, cancel)

	for _, n := range networks {
		dup := w
		dup.network = n

		if err := t.arena.Run(ctx, dup); err != nil {
			return nil, err
		}
	}

	<-ctx.Done()
	return *w.fastest.Load(), errorsx.Compact(langx.Autoderef(w.failed.Load()), context.Cause(ctx))
}
