package proxy

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestActivityBusyUnknownAndPendingWorkRefuseSleep(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	a := NewActivity(func() time.Time { return time.Unix(0, clock.Load()) }, false)
	stops := 0
	stop := func() error { stops++; return nil }
	busy := func() (bool, error) { return true, nil }
	idle := func() (bool, error) { return false, nil }
	for range 3 {
		clock.Add(int64(time.Hour))
		if err := a.Check(time.Minute, busy, stop); err != nil {
			t.Fatal(err)
		}
	}
	clock.Add(int64(time.Hour))
	if err := a.Check(time.Minute, func() (bool, error) { return false, errors.New("unknown") }, stop); err == nil {
		t.Fatal("unknown not reported")
	}
	done := a.Begin()
	clock.Add(int64(time.Hour))
	if err := a.Check(time.Minute, idle, stop); err != nil {
		t.Fatal(err)
	}
	done()
	done() // release is idempotent
	if err := a.Check(time.Minute, idle, stop); err != nil {
		t.Fatal(err)
	}
	if stops != 0 {
		t.Fatal("stopped with work or without grace")
	}
	clock.Add(int64(time.Minute))
	if err := a.Check(time.Minute, idle, stop); err != nil {
		t.Fatal(err)
	}
	if stops != 1 || !a.Sleeping() {
		t.Fatal("confirmed idle did not sleep")
	}
}

func TestActivityGateSerializesNewWorkWithStop(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	a := NewActivity(func() time.Time { return time.Unix(0, clock.Load()) }, false)
	clock.Add(int64(time.Hour))
	stopping := make(chan struct{})
	finishStop := make(chan struct{})
	checked := make(chan error, 1)
	go func() {
		checked <- a.Check(time.Minute, func() (bool, error) { return false, nil }, func() error { close(stopping); <-finishStop; return nil })
	}()
	<-stopping
	started := make(chan func(), 1)
	go func() { started <- a.Begin() }()
	select {
	case <-started:
		t.Fatal("request passed the final stop gate")
	case <-time.After(20 * time.Millisecond):
	}
	close(finishStop)
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	select {
	case done := <-started:
		done()
	case <-time.After(time.Second):
		t.Fatal("work remained blocked after stop")
	}
}

func TestPassiveTransportDoesNotResetIdleClock(t *testing.T) {
	now := time.Now()
	a := NewActivity(func() time.Time { return now }, false)
	now = now.Add(time.Hour)
	done, ok := a.BeginPassive()
	if !ok {
		t.Fatal("running transport refused")
	}
	done()
	if err := a.Check(time.Minute, func() (bool, error) { return false, nil }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !a.Sleeping() {
		t.Fatal("background transport reset idle window")
	}
	if _, ok := a.BeginPassive(); ok {
		t.Fatal("passive request can wake sleeping VM")
	}
}
