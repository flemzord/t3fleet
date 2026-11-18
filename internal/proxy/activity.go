package proxy

import (
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
	sockets  map[uint64]func()
	next     uint64
}

func NewActivity(now func() time.Time, sleeping bool) *Activity {
	return &Activity{now: now, last: now(), sleeping: sleeping, sockets: map[uint64]func(){}}
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
func (a *Activity) Awake()         { a.mu.Lock(); a.sleeping = false; a.last = a.now(); a.mu.Unlock() }

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
	a.sleeping = true
	// Cancellation is nonblocking. New sockets remain local until useful work arrives.
	for _, cancel := range a.sockets {
		cancel()
	}
	return nil
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
	a.sleeping = true
	for _, cancel := range a.sockets {
		cancel()
	}
}
