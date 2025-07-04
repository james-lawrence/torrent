package cstate

import (
	"context"
	"sync"
	"testing"
	"time"

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
		require.False(t, i.running.Load()) // Should be initialized to false
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

		// Give Update a moment to start and set running to true
		time.Sleep(50 * time.Millisecond)
		require.True(t, idleInstance.running.Load())

		// Signal one of the conditions
		signalCond1.Broadcast()

		// Wait for Update to complete
		select {
		case <-updateDone:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Update did not complete after signal")
		}
		require.False(t, idleInstance.running.Load()) // running should be false after defer
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
		require.False(t, idleInstance.running.Load()) // running should be false after defer
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
		require.True(t, idleInstance.running.Load())

		// Cancel the context
		cancel()

		// Wait for Update to complete
		select {
		case <-updateDone:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Update did not complete after context cancellation")
		}
		require.False(t, idleInstance.running.Load()) // running should be false after defer
	})

	t.Run("target.Broadcast does not signal done channel if running is false", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond := sync.NewCond(&sync.Mutex{})
		idleInstance := Idle(ctx, targetCond, signalCond)

		// Ensure running is false (it is by default, but explicitly set for clarity)
		idleInstance.running.Store(false)

		// Signal a condition, which should cause target.Broadcast()
		signalCond.Broadcast()

		// Give some time for the goroutines to process
		time.Sleep(100 * time.Millisecond)

		// Try to read from done channel, it should block or not receive
		select {
		case <-idleInstance.done:
			t.Fatal("done channel received a signal when running was false")
		case <-time.After(50 * time.Millisecond):
			// Expected: done channel did not receive a signal
		}
	})

	t.Run("target.Broadcast signals done channel if running is true", func(t *testing.T) {
		targetCond := sync.NewCond(&sync.Mutex{})
		signalCond := sync.NewCond(&sync.Mutex{})
		idler := Idle(t.Context(), targetCond, signalCond)

		// Set running to true
		idler.running.Store(true)

		// short delay to ensure we've blocked.
		time.Sleep(100 * time.Millisecond)
		// Signal a condition, which should cause target.Broadcast()
		signalCond.Broadcast()

		// Expect to receive from done channel
		select {
		case <-idler.done:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("done channel did not receive a signal when running was true")
		}
	})
}
