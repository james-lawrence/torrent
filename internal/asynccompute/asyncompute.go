package asynccompute

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/james-lawrence/torrent/internal/langx"
)

// pool of workers
type Pool[T any] struct {
	workers  int
	shutdown sync.WaitGroup // track active compute routines
	async    func(ctx context.Context, w T) error
	queued   chan pending
	failed   atomic.Pointer[error]
}

func (t *Pool[T]) Run(ctx context.Context, w T) error {
	select {
	case t.queued <- pending{workload: func() error { return t.async(ctx, w) }}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (t *Pool[T]) Close() error {
	close(t.queued)
	t.shutdown.Wait()
	return langx.Autoderef(t.failed.Load())
}

func (t *Pool[T]) init() *Pool[T] {
	t.shutdown.Add(int(t.workers))
	for i := 0; i < t.workers; i++ {
		go func() {
			defer t.shutdown.Done()
			for pending := range t.queued {
				t.failed.CompareAndSwap(nil, langx.Autoptr(errorsx.LogErr(pending.workload())))
			}
		}()
	}

	return t
}

type pending struct {
	workload func() error
}

type Option[T any] func(*Pool[T])

func Backlog[T any](n uint16) Option[T] {
	return func(p *Pool[T]) {
		p.queued = make(chan pending, n)
	}
}

func Workers[T any](n uint16) Option[T] {
	return func(p *Pool[T]) {
		p.workers = int(n)
	}
}

func Compose[T any](options ...Option[T]) Option[T] {
	return func(p *Pool[T]) {
		for _, opt := range options {
			opt(p)
		}
	}
}

func New[T any](async func(ctx context.Context, w T) error, options ...Option[T]) *Pool[T] {
	return langx.Autoptr(langx.Clone(Pool[T]{
		workers: runtime.NumCPU(),
		queued:  make(chan pending, runtime.NumCPU()),
		async:   async,
	}, options...)).init()
}

// gracefully shutdown by invoking close and waiting until all workers
// complete or the context times out.
func Shutdown[T any](ctx context.Context, p *Pool[T]) error {
	dctx, cancelled := context.WithCancelCause(ctx)
	go func() {
		cancelled(p.Close())
	}()

	<-dctx.Done()
	return errorsx.Ignore(context.Cause(dctx), context.Canceled)
}
