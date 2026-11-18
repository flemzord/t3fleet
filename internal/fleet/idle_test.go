package fleet_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type idleFixture struct {
	*fixture
	clock atomic.Int64
}

func newIdleFleet(t *testing.T) *idleFixture {
	t.Helper()
	f := &idleFixture{fixture: newFleet(t)}
	f.clock.Store(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).UnixNano())
	f.options.Now = func() time.Time { return time.Unix(0, f.clock.Load()) }
	f.options.IdleTimeout = time.Minute
	f.options.IdleCheckInterval = time.Hour
	f.restart(t)
	return f
}
func (f *idleFixture) advance(d time.Duration) { f.clock.Add(int64(d)) }
func (f *idleFixture) setShell(body string)    { f.p.mu.Lock(); f.p.shell = body; f.p.mu.Unlock() }
func (f *idleFixture) stopCount() int          { f.p.mu.Lock(); defer f.p.mu.Unlock(); return f.p.stops }

const idleShell = `{"threads":[{"status":"idle","activeRunId":null}],"archivedThreads":[]}`

func TestIdleStopPreservesPairingAndWakesOnlyForUsefulRequests(t *testing.T) {
	f := newIdleFleet(t)
	machine := create(t, f.fixture, "idle-worker")
	token := pair(t, f.fixture, machine)
	f.setShell(idleShell)
	f.advance(59 * time.Second)
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stopCount() != 0 {
		t.Fatal("stopped before idle timeout")
	}
	f.advance(time.Second)
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped, _ := f.m.Get(machine.ID)
	if stopped.State != "stopped" || stopped.RemoteID != machine.RemoteID || stopped.URL != machine.URL || stopped.LastStopReason != "idle-timeout" || stopped.LastStoppedAt.IsZero() {
		t.Fatal("lost stop state", stopped)
	}
	f.restart(t)
	for _, path := range []string{"/.well-known/t3/environment", "/api/auth/session"} {
		if status, _ := request(t, machine.URL+path, token); status != 200 {
			t.Fatal("sleeping metadata unavailable", path, status)
		}
	}
	if status, _ := request(t, machine.URL+"/api/orchestration/shell", token); status != 503 {
		t.Fatal("sleeping shell should not wake", status)
	}
	if status, _ := request(t, machine.URL+"/api/probe", "foreign-token"); status != 401 {
		t.Fatal("foreign token accepted", status)
	}
	f.p.mu.Lock()
	starts := f.p.starts
	f.p.mu.Unlock()
	if starts != 0 {
		t.Fatal("background traffic woke VM")
	}
	if status, _ := request(t, machine.URL+"/api/probe", token); status != 200 {
		t.Fatal("useful request failed to wake", status)
	}
	f.p.mu.Lock()
	starts = f.p.starts
	allocations := f.p.allocations
	f.p.mu.Unlock()
	if starts != 1 || allocations != 1 {
		t.Fatal("wake replaced or duplicated VM", starts, allocations)
	}
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stopCount() != 1 {
		t.Fatal("no grace period after wake")
	}
}

func TestIdleNeverStopsBusyOrUnknownAgents(t *testing.T) {
	for _, body := range []string{
		`{"threads":[{"status":"running","activeRunId":null}],"archivedThreads":[]}`,
		`{"threads":[{"status":"queued","activeRunId":null}],"archivedThreads":[]}`,
		`{"threads":[{"status":"idle","activeRunId":null,"blockedOnThreadWait":true}],"archivedThreads":[]}`,
		`{"threads":[{"status":"idle","activeRunId":"run"}],"archivedThreads":[]}`,
		`{"threads":[{"status":"idle","activeRunId":null,"activityRunStatus":"waiting"}],"archivedThreads":[]}`,
		`{"threads":[{"status":"idle","activeRunId":null,"pendingRuntimeRequest":{"id":"approval"}}],"archivedThreads":[]}`,
		`{"threads":[],"archivedThreads":[{"status":"running","activeRunId":"subagent","lineage":{"relationshipToParent":"subagent"}}]}`,
		`{"threads":[{"status":"idle","activeRunId":null,"pendingBackgroundTasks":[{}]}],"archivedThreads":[]}`,
		`{"threads":[{"status":"future-status","activeRunId":null}],"archivedThreads":[]}`,
		`{"threads":[{"status":"idle"}],"archivedThreads":[]}`,
		`{"threads":[],"archivedThreads":null}`, `{"resumable":true}`, `null`, `not-json`,
	} {
		t.Run(fmt.Sprintf("shell-%s", body), func(t *testing.T) {
			f := newIdleFleet(t)
			create(t, f.fixture, "busy-worker")
			f.setShell(body)
			for range 3 {
				f.advance(time.Hour)
				_ = f.m.CheckIdle(t.Context())
			}
			if f.stopCount() != 0 {
				t.Fatal("stopped busy/unknown agent")
			}
			f.setShell(idleShell)
			if err := f.m.CheckIdle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.stopCount() != 0 {
				t.Fatal("did not reset idle window after busy/unknown")
			}
			f.advance(time.Minute)
			if err := f.m.CheckIdle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.stopCount() != 1 {
				t.Fatal("never stopped confirmed-idle worker")
			}
		})
	}
}

func TestIdleFinalRecheckRefusesNewAgentAndFailedStopRetainsState(t *testing.T) {
	f := newIdleFleet(t)
	machine := create(t, f.fixture, "recheck")
	f.advance(time.Hour)
	f.p.mu.Lock()
	f.p.shellReplies = []string{idleShell, `{"threads":[{"status":"running","activeRunId":"new-run"}],"archivedThreads":[]}`}
	f.p.mu.Unlock()
	if err := f.m.CheckIdle(t.Context()); err == nil {
		t.Fatal("final busy check should refuse stop")
	}
	if f.stopCount() != 0 {
		t.Fatal("stopped newly busy agent")
	}
	f.setShell(idleShell)
	f.advance(time.Hour)
	f.p.mu.Lock()
	f.p.stopFails = true
	f.p.mu.Unlock()
	if err := f.m.CheckIdle(t.Context()); err == nil {
		t.Fatal("lost stop error")
	}
	current, _ := f.m.Get(machine.ID)
	if current.State != "ready" || current.LastStopReason != "" || current.RemoteID != machine.RemoteID {
		t.Fatal("failed stop corrupted record", current)
	}
	f.p.mu.Lock()
	f.p.stopFails = false
	f.p.mu.Unlock()
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stopCount() != 1 {
		t.Fatal("failed stop retried without idle grace")
	}
	f.advance(time.Minute)
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stopCount() != 2 {
		t.Fatal("stop recovery not attempted")
	}
}

func TestIdleDisabledAndCancellationDoNotStop(t *testing.T) {
	f := newFleet(t)
	create(t, f, "always-on")
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.stops != 0 || f.p.shellReads != 0 {
		t.Fatal("idle policy enabled by default")
	}
}
func TestCancelledIdleSweepDoesNotStop(t *testing.T) {
	f := newIdleFleet(t)
	create(t, f.fixture, "cancel-idle")
	f.advance(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.m.CheckIdle(ctx); err == nil {
		t.Fatal("cancelled sweep succeeded")
	}
	if f.stopCount() != 0 {
		t.Fatal("cancelled sweep stopped VM")
	}
}

func TestIdleProbeFailureIsSafeAndRedacted(t *testing.T) {
	f := newIdleFleet(t)
	create(t, f.fixture, "probe-fail")
	f.advance(time.Hour)
	f.setShell(strings.Repeat("private-token", 400000))
	err := f.m.CheckIdle(t.Context())
	if err == nil || strings.Contains(err.Error(), "private-token") || f.stopCount() != 0 {
		t.Fatal("unsafe probe failure")
	}
	// The cached client session still requires its own credential.
	machine := f.m.List()[0]
	if status, _ := request(t, machine.URL+"/api/auth/session", ""); status != http.StatusUnauthorized {
		t.Fatal("metadata lost auth")
	}
}

func TestIdleLoopStopsWithoutExplicitSweepAndExitsOnClose(t *testing.T) {
	f := newIdleFleet(t)
	f.options.IdleCheckInterval = 10 * time.Millisecond
	f.restart(t)
	create(t, f.fixture, "background-idle")
	f.advance(time.Minute)
	wait, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for f.stopCount() == 0 {
		select {
		case <-ticker.C:
		case <-wait.Done():
			t.Fatal("background idle loop did not stop VM")
		}
	}
	if err := f.m.Close(wait); err != nil {
		t.Fatal("idle daemon shutdown failed", err)
	}
}

func TestIdleProbeDoesNotWakeAnExternallyStoppedVM(t *testing.T) {
	f := newIdleFleet(t)
	machine := create(t, f.fixture, "external-stop")
	f.p.mu.Lock()
	remote := f.p.machines[machine.RemoteID]
	remote.State = "stopped"
	f.p.machines[machine.RemoteID] = remote
	f.p.mu.Unlock()
	f.advance(time.Hour)
	if err := f.m.CheckIdle(t.Context()); err == nil {
		t.Fatal("external state change was not treated as unknown")
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.starts != 0 || f.p.stops != 0 || f.p.shellReads != 0 {
		t.Fatal("activity probe changed cloud lifecycle")
	}
}

func TestPeriodicServiceRevalidationDoesNotResetIdleWindow(t *testing.T) {
	f := newIdleFleet(t)
	f.options.IdleTimeout = 30 * time.Minute
	f.restart(t)
	machine := create(t, f.fixture, "passive-auth")
	token := pair(t, f.fixture, machine)
	f.advance(11 * time.Minute)
	if status, _ := request(t, machine.URL+"/api/auth/session", token); status != 200 {
		t.Fatal("background revalidation failed", status)
	}
	f.advance(19 * time.Minute)
	if err := f.m.CheckIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stopCount() != 1 {
		t.Fatal("periodic auth revalidation extended idle timeout")
	}
}

func TestIdleAgentProbeRequiresSuccessfulAuthorization(t *testing.T) {
	for _, status := range []int{401, 403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newIdleFleet(t)
			create(t, f.fixture, "denied-probe")
			f.advance(time.Hour)
			f.p.mu.Lock()
			f.p.shellStatus = status
			f.p.mu.Unlock()
			if err := f.m.CheckIdle(t.Context()); err == nil || f.stopCount() != 0 {
				t.Fatal("failed agent authorization permitted stop", status, err)
			}
		})
	}
}
