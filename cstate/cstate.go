package cstate

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/james-lawrence/torrent/internal/atomicx"
	"github.com/james-lawrence/torrent/internal/langx"
)

type logger interface {
	Println(v ...interface{})
	Printf(format string, v ...interface{})
	Print(v ...interface{})
}
type Shared struct {
	done context.CancelCauseFunc
	log  logger
}

type T interface {
	Update(context.Context, *Shared) T
}

func Idle(ctx context.Context, cond *sync.Cond, signals ...*sync.Cond) *Idler {
	tt := time.NewTimer(time.Hour)
	tt.Stop()

	return (&Idler{
		timeout: tt,
		target:  cond,
		signals: signals,
		done:    make(chan struct{}),
		running: langx.Autoderef(atomicx.Bool(false)),
	}).monitor(ctx)
}

type Idler struct {
	timeout *time.Timer
	target  *sync.Cond
	signals []*sync.Cond
	done    chan struct{}
	running atomic.Bool
}

func (t *Idler) Idle(next T, d time.Duration) idle {
	if d > 0 {
		t.timeout.Reset(d)
	}
	return idle{Idler: t, next: next}
}

func (t *Idler) monitor(ctx context.Context) *Idler {
	for _, s := range t.signals {
		go func() {
			for {
				s.L.Lock()
				s.Wait()
				s.L.Unlock()
				t.target.Broadcast()
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}()
	}

	go func() {
		for {
			t.target.L.Lock()
			t.target.Wait()
			t.target.L.Unlock()

			if !t.running.Load() {
				continue
			}

			select {
			case t.done <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return t
}

type idle struct {
	*Idler
	next T
}

func (t idle) Update(ctx context.Context, c *Shared) T {
	defer t.Idler.running.Store(false)
	defer t.Idler.timeout.Stop()
	t.Idler.running.Store(true)
	select {
	case <-t.done:
	case <-t.Idler.timeout.C:
	case <-ctx.Done():
	}
	return t.next
}

func (t idle) String() string {
	return fmt.Sprintf("%T - %s - idle", t.next, t.next)
}

func Failure(cause error) failed {
	return failed{cause: cause}
}

type failed struct {
	cause error
}

func (t failed) Update(ctx context.Context, c *Shared) T {
	c.done(t.cause)
	return nil
}

func (t failed) String() string {
	return fmt.Sprintf("%T - %s", t, t.cause)
}

func Warning(next T, cause error) warning {
	return warning{next: next, cause: cause}
}

type warning struct {
	cause error
	next  T
}

func (t warning) Update(ctx context.Context, c *Shared) T {
	c.log.Println("[warning]", t.cause)
	return t.next
}

func (t warning) String() string {
	return fmt.Sprintf("%T - %T", t, t.cause)
}

func Halt() halt {
	return halt{}
}

type halt struct{}

func (t halt) Update(ctx context.Context, c *Shared) T {
	c.done(nil)
	return nil
}
func Fn(fn fn) fn {
	return fn
}

type fn func(context.Context, *Shared) T

func (t fn) Update(ctx context.Context, s *Shared) T {
	return t(ctx, s)
}

func (t fn) String() string {
	pc := reflect.ValueOf(t).Pointer()
	info := runtime.FuncForPC(pc)
	fname, line := info.FileLine(pc)
	return fmt.Sprintf("%s:%d", fname, line)
}

func Run(ctx context.Context, s T, l logger) error {
	ctx, cancelled := context.WithCancelCause(ctx)
	var (
		m = Shared{
			done: cancelled,
			log:  l,
		}
	)

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			l.Printf("%s - %T\n", s, s)
			s = s.Update(ctx, &m)
		}

		if s == nil {
			return context.Cause(ctx)
		}
	}
}
