package proxy

import (
	"context"
	"sync"
	"time"
)

// Activity serializes useful client work with the final idle check and stop.
// Transport pings and known background subscriptions do not count as work.
type Activity struct {
	mu       sync.Mutex
	now      func() time.Time
	last     time.Time
	active   int
	sleeping bool
	wakeup   chan struct{}
	sockets  map[uint64]func()
	next     uint64
}

func NewActivity(now func() time.Time, sleeping bool) *Activity {
	a := &Activity{now: now, last: now(), sleeping: sleeping, sockets: map[uint64]func(){}}
	if sleeping {
		a.wakeup = make(chan struct{})
	}
	return a
}
func (a *Activity) Begin() func() {
	a.mu.Lock()
	a.active++
	a.last = a.now()
	a.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.active--; a.last = a.now(); a.mu.Unlock() }) }
}
func (a *Activity) Sleeping() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.sleeping }
func (a *Activity) Awake() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sleeping {
		close(a.wakeup)
		a.wakeup = nil
		a.sleeping = false
	}
	a.last = a.now()
}

func (a *Activity) waitAwake(ctx context.Context) error {
	a.mu.Lock()
	wakeup := a.wakeup
	a.mu.Unlock()
	if wakeup == nil {
		return ctx.Err()
	}
	select {
	case <-wakeup:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Check holds new useful work outside the upstream until the stop completes.
// Any failure or observed work restarts a full idle window. Unknown never means idle.
func (a *Activity) Check(timeout time.Duration, probe func() (bool, error), stop func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sleeping || a.active > 0 {
		return nil
	}
	busy, err := probe()
	if err != nil || busy {
		a.last = a.now()
		return err
	}
	if a.now().Sub(a.last) < timeout {
		return nil
	}
	if err := stop(); err != nil {
		a.last = a.now()
		return err
	}
	a.suspendLocked()
	return nil
}

func (a *Activity) suspendLocked() {
	if !a.sleeping {
		a.sleeping = true
		a.wakeup = make(chan struct{})
	}
	// Cancellation is nonblocking. New sockets remain local until useful work arrives.
	for _, cancel := range a.sockets {
		cancel()
	}
}
func (a *Activity) watch(cancel func()) func() {
	a.mu.Lock()
	a.next++
	id := a.next
	a.sockets[id] = cancel
	a.mu.Unlock()
	return func() { a.mu.Lock(); delete(a.sockets, id); a.mu.Unlock() }
}

// BeginPassive protects a finite transport operation without extending idleness.
func (a *Activity) BeginPassive() (func(), bool) {
	a.mu.Lock()
	if a.sleeping {
		a.mu.Unlock()
		return nil, false
	}
	a.active++
	a.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.active--; a.mu.Unlock() }) }, true
}
func (a *Activity) Suspend() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.suspendLocked()
}
