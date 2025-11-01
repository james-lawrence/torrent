// Package chansync is cloned from github.com/anacrolix/chansync to reduce dependency tree.
package chansync

import (
	"sync"
	"sync/atomic"
)

type (
	Signaled <-chan struct{}
	Done     <-chan struct{}
	Active   <-chan struct{}
	Signal   chan<- struct{}
	Acquire  chan<- struct{}
	Release  <-chan struct{}
)

// Can be used as zero-value. Due to the caller needing to bring their own synchronization, an
// equivalent to "sync".Cond.Signal is not provided. BroadcastCond is intended to be selected on
// with other channels.
type BroadcastCond struct {
	mu sync.Mutex
	ch chan struct{}
}

func (me *BroadcastCond) Broadcast() {
	me.mu.Lock()
	defer me.mu.Unlock()
	if me.ch != nil {
		close(me.ch)
		me.ch = nil
	}
}

// Should be called before releasing locks on resources that might trigger subsequent Broadcasts.
// The channel is closed when the condition changes.
func (me *BroadcastCond) Signaled() Signaled {
	me.mu.Lock()
	defer me.mu.Unlock()
	if me.ch == nil {
		me.ch = make(chan struct{})
	}
	return me.ch
}

type LevelTrigger struct {
	ch       chan struct{}
	initOnce sync.Once
}

func (me *LevelTrigger) Signal() Signal {
	me.init()
	return me.ch
}

func (me *LevelTrigger) Active() Active {
	me.init()
	return me.ch
}

func (me *LevelTrigger) init() {
	me.initOnce.Do(func() {
		me.ch = make(chan struct{})
	})
}

// SetOnce is a boolean value that can only be flipped from false to true.
type SetOnce struct {
	ch chan struct{}
	// Could be faster than trying to receive from ch
	closed    atomic.Bool
	initOnce  sync.Once
	closeOnce sync.Once
}

// Returns a channel that is closed when the event is flagged.
func (me *SetOnce) Done() Done {
	me.init()
	return me.ch
}

func (me *SetOnce) init() {
	me.initOnce.Do(func() {
		me.ch = make(chan struct{})
	})
}

// Set only returns true the first time it is called.
func (me *SetOnce) Set() (first bool) {
	me.closeOnce.Do(func() {
		me.init()
		first = true
		me.closed.Store(true)
		close(me.ch)
	})
	return
}

func (me *SetOnce) IsSet() bool {
	return me.closed.Load()
}
