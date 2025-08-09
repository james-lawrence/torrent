package cstate

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type testLogger struct {
	t *testing.T
}

func (tl testLogger) Print(v ...interface{}) {
	tl.t.Log(v...)
}

func (tl testLogger) Printf(format string, v ...interface{}) {
	tl.t.Logf(format, v...)
}

func (tl testLogger) Println(v ...interface{}) {
	tl.t.Log(v...)
}

type captureLogger struct {
	testLogger
	captured []string
}

func (cl *captureLogger) Println(v ...interface{}) {
	cl.captured = append(cl.captured, strings.TrimSuffix(fmt.Sprintln(v...), "\n"))
	cl.testLogger.Println(v...)
}

// TestRunHalt verifies that Halt stops the state machine without error.
func TestRunHalt(t *testing.T) {
	var (
		ctx = context.Background()
		s   = Halt()
		l   = testLogger{t}
	)

	err := Run(ctx, s, l)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestRunFailure verifies that Failure cancels with the given error.
func TestRunFailure(t *testing.T) {
	var (
		expected = errors.New("test failure")
		ctx      = context.Background()
		s        = Failure(expected)
		l        = testLogger{t}
	)

	err := Run(ctx, s, l)
	if !errors.Is(err, expected) {
		t.Errorf("expected error %v, got %v", expected, err)
	}
}

// TestRunWarning verifies that Warning logs the error and proceeds.
func TestRunWarning(t *testing.T) {
	var (
		warnErr  = errors.New("test warning")
		ctx      = context.Background()
		cl       = &captureLogger{testLogger: testLogger{t}}
		s        = Warning(Halt(), warnErr)
		expected = "[warning] test warning"
	)

	err := Run(ctx, s, cl)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if len(cl.captured) != 1 || cl.captured[0] != expected {
		t.Errorf("expected captured log %q, got %v", expected, cl.captured)
	}
}

// TestRunFn verifies that Fn executes the function and returns next state.
func TestRunFn(t *testing.T) {
	var (
		ctx = context.Background()
		l   = testLogger{t}
		fn  = Fn(func(context.Context, *Shared) T {
			return Halt()
		})
	)

	err := Run(ctx, fn, l)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestIdleTimeout verifies that idle times out and proceeds.
func TestIdleTimeout(t *testing.T) {
	var (
		mu    sync.Mutex
		cond  = sync.NewCond(&mu)
		ctx   = context.Background()
		idler = Idle(ctx, cond)
		d     = 50 * time.Millisecond
		s     = idler.Idle(Halt(), d)
		l     = testLogger{t}
		start = time.Now()
	)

	err := Run(ctx, s, l)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < d {
		t.Errorf("expected elapsed >= %v, got %v", d, elapsed)
	}
}

// TestIdleSignal verifies that signaling wakes the idler.
func TestIdleSignal(t *testing.T) {
	var (
		mu     sync.Mutex
		cond   = sync.NewCond(&mu)
		signal = sync.NewCond(&mu)
		ctx    = context.Background()
		idler  = Idle(ctx, cond, signal)
		d      = time.Hour
		s      = idler.Idle(Halt(), d)
		l      = testLogger{t}
		done   = make(chan error)
	)

	go func() {
		done <- Run(ctx, s, l)
	}()

	time.Sleep(50 * time.Millisecond)
	signal.Broadcast()
	err := <-done
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestIdleCancel verifies that context cancel wakes the idler.
func TestIdleCancel(t *testing.T) {
	var (
		mu       sync.Mutex
		cond     = sync.NewCond(&mu)
		ctx, cxl = context.WithCancel(context.Background())
		idler    = Idle(ctx, cond)
		d        = time.Hour
		s        = idler.Idle(Halt(), d)
		l        = testLogger{t}
		done     = make(chan error)
	)

	go func() {
		done <- Run(ctx, s, l)
	}()

	time.Sleep(50 * time.Millisecond)
	cxl()
	err := <-done
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestIdlerNoLeak verifies no goroutine leaks after cancel.
func TestIdlerNoLeak(t *testing.T) {
	var (
		iterations = 100
		tolerance  = 10
		before     = runtime.NumGoroutine()
	)
	for i := 0; i < iterations; i++ {
		var (
			mu       sync.Mutex
			cond     = sync.NewCond(&mu)
			ctx, cxl = context.WithCancel(context.Background())
			idler    = Idle(ctx, cond)
			d        = time.Hour
			s        = idler.Idle(Halt(), d)
			l        = testLogger{t}
			done     = make(chan struct{})
		)

		go func() {
			Run(ctx, s, l)
			close(done)
		}()

		time.Sleep(10 * time.Millisecond)
		cxl()
		<-done
	}

	after := runtime.NumGoroutine()
	delta := after - before
	if delta > tolerance {
		t.Errorf("expected goroutine delta <= %d, got %d", tolerance, delta)
	}
}
