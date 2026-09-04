package cstate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/internal/testx"
	"github.com/stretchr/testify/require"
)

func TestIdleFunction(t *testing.T) {
	t.Run("initializes _idle struct correctly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond1 := sync.NewCond(&sync.Mutex{})
		signalCond2 := sync.NewCond(&sync.Mutex{})

		i := Idle(ctx, targetCond, signalCond1, signalCond2)

		require.NotNil(t, i)
		require.NotNil(t, i.timeout)
		require.Equal(t, targetCond, i.target)
		require.Len(t, i.signals, 2)
		require.Equal(t, signalCond1, i.signals[0])
		require.Equal(t, signalCond2, i.signals[1])
		require.NotNil(t, i.done)
	})
}

func TestIdleMethod(t *testing.T) {
	t.Run("resets timeout when duration is positive", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		idleInstance := Idle(ctx, targetCond)

		newDuration := 100 * time.Millisecond
		returnedIdle := idleInstance.Idle(Halt(), newDuration)

		require.NotNil(t, returnedIdle.Idler)
		require.Equal(t, idleInstance, returnedIdle.Idler)
		require.NotNil(t, returnedIdle.next)
	})

	t.Run("does not reset timeout when duration is zero", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		idleInstance := Idle(ctx, targetCond)

		initialTimeoutC := idleInstance.timeout.C // Store initial channel

		returnedIdle := idleInstance.Idle(Halt(), 0)

		require.NotNil(t, returnedIdle.Idler)
		require.Equal(t, idleInstance, returnedIdle.Idler)
		require.NotNil(t, returnedIdle.next)

		// If timeout was stopped initially, and not reset, its channel should remain the same.
		require.Equal(t, initialTimeoutC, idleInstance.timeout.C)
	})
}

func TestIdleUpdate(t *testing.T) {
	t.Run("returns next when a signal condition is met", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond1 := sync.NewCond(&sync.Mutex{})
		signalCond2 := sync.NewCond(&sync.Mutex{})

		idleInstance := Idle(ctx, targetCond, signalCond1, signalCond2)

		expectedNext := Halt()
		i := idleInstance.Idle(expectedNext, 0) // No timeout for this test

		updateDone := make(chan struct{})
		go func() {
			returnedNext := i.Update(ctx, &Shared{})
			require.Equal(t, expectedNext, returnedNext)
			close(updateDone)
		}()

		// Give Update a moment to start
		time.Sleep(50 * time.Millisecond)

		// Signal one of the conditions
		signalCond1.Broadcast()

		// Wait for Update to complete
		select {
		case <-updateDone:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Update did not complete after signal")
		}
	})

	t.Run("returns next when timeout occurs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		idleInstance := Idle(ctx, targetCond) // No signals for this test

		testDuration := 100 * time.Millisecond
		expectedNext := Halt()
		i := idleInstance.Idle(expectedNext, testDuration) // Set a short timeout

		updateDone := make(chan struct{})
		startTime := time.Now()
		go func() {
			returnedNext := i.Update(ctx, &Shared{})
			require.Equal(t, expectedNext, returnedNext)
			close(updateDone)
		}()

		// Wait for Update to complete
		select {
		case <-updateDone:
			elapsed := time.Since(startTime)
			require.True(t, elapsed >= testDuration, "Update returned too early (elapsed: %v, expected: %v)", elapsed, testDuration)
		case <-time.After(testDuration + 200*time.Millisecond): // Add buffer for goroutine scheduling
			t.Fatal("Update did not complete after timeout")
		}
	})

	t.Run("returns next when context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond := sync.NewCond(&sync.Mutex{}) // Include a signal to ensure it's not the cause
		idleInstance := Idle(ctx, targetCond, signalCond)

		expectedNext := Halt()
		i := idleInstance.Idle(expectedNext, 0) // No timeout for this test

		updateDone := make(chan struct{})
		go func() {
			returnedNext := i.Update(ctx, &Shared{})
			require.Equal(t, expectedNext, returnedNext)
			close(updateDone)
		}()

		// Give Update a moment to start
		time.Sleep(50 * time.Millisecond)

		// Cancel the context
		cancel()

		// Wait for Update to complete
		select {
		case <-updateDone:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Update did not complete after context cancellation")
		}
	})

	t.Run("a wakeup that arrives before Update is called is not lost", func(t *testing.T) {
		// A Broadcast that arrives while nothing has called idle.Update() yet
		// must not be dropped - it needs to be durably queued so the very next Update()
		// call sees it immediately, rather than blocking for its full timeout.
		ctx, cancel := testx.Context(t)
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond := sync.NewCond(&sync.Mutex{})
		idleInstance := Idle(ctx, targetCond, signalCond)

		// Give the monitor goroutines time to actually reach Wait() before
		// broadcasting, so a failure here can only be the delivery logic's
		// fault, not scheduling luck.
		time.Sleep(100 * time.Millisecond)

		signalCond.Broadcast()

		// Give the broadcast time to propagate: signal's monitor goroutine
		// wakes, relays via target.Broadcast(), target's monitor goroutine
		// wakes and queues into done.
		time.Sleep(100 * time.Millisecond)

		expectedNext := Halt()
		i := idleInstance.Idle(expectedNext, time.Second) // long timeout - must not be what fires below

		start := time.Now()
		returnedNext := i.Update(ctx, &Shared{})
		require.Equal(t, expectedNext, returnedNext)
		require.Less(t, time.Since(start), 200*time.Millisecond, "the queued wakeup from before Update was called must be delivered immediately, not lost until the timeout")
	})
}
