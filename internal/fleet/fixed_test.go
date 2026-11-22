package fleet_test

import (
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestFixedWorkerReusesSlotAcrossProjectsAndRestart(t *testing.T) {
	f, boat, _ := dispatchProviders(t)
	slot := strings.Repeat("a", 32)
	first := strings.Repeat("b", 32)
	second := strings.Repeat("c", 32)
	const remote = "https://github.com/octocat/Hello-World.git"
	if got := f.m.FixedWorkerState(slot); got != "absent" {
		t.Fatalf("unallocated slot reports %s", got)
	}
	boat.onCreate = func() {
		if got := f.m.FixedWorkerState(slot); got != "stopped" {
			t.Fatalf("fixed slot was not journaled before provider create: %s", got)
		}
	}
	worker, cwd, err := f.m.ProvisionFixedWorker(t.Context(), slot, first, remote, []string{"boat"})
	if err != nil || !strings.HasSuffix(cwd, "/"+first+"/repo") {
		t.Fatalf("first checkout: %s, %v", cwd, err)
	}
	if got := f.m.FixedWorkerState(slot); got != "ready" {
		t.Fatalf("initialised slot reports %s", got)
	}
	if err := f.m.ReleaseDispatchWorker(t.Context(), slot); err == nil || boat.deletes != 0 {
		t.Fatal("dedicated worker cleanup accepted a shared slot")
	}
	f.restart(t)
	again, err := f.m.LookupFixedWorker(slot)
	if err != nil || again.ID != worker.ID {
		t.Fatalf("restart lost the fixed worker: %v", err)
	}
	otherProject, otherCWD, err := f.m.ProvisionFixedWorker(t.Context(), slot, second, remote, []string{"boat"})
	if err != nil || otherProject.ID != worker.ID || otherCWD == cwd ||
		!strings.HasSuffix(otherCWD, "/"+second+"/repo") || boat.allocations != 1 {
		t.Fatalf("second project did not reuse the VM with a distinct checkout: %s, %v", otherCWD, err)
	}
	if err := f.m.Delete(t.Context(), worker.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.m.FixedWorkerState(slot); got != "deleted" {
		t.Fatalf("explicitly deleted worker lost its tombstone: %s", got)
	}
	if _, _, err := f.m.ProvisionFixedWorker(t.Context(), slot, second, remote, []string{"boat"}); err == nil || boat.allocations != 1 {
		t.Fatal("tombstoned slot silently reallocated a machine")
	}
}

func TestFixedWorkerReconcilesUncertainCreateWithoutChangingProvider(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	slot := strings.Repeat("d", 32)
	checkout := strings.Repeat("e", 32)
	const remote = "https://github.com/octocat/Hello-World.git"
	boat.createFailsOnce = true
	if _, _, err := f.m.ProvisionFixedWorker(t.Context(), slot, checkout, remote, []string{"boat", "namespace"}); err == nil {
		t.Fatal("expected a lost provider response")
	}
	f.restart(t)
	worker, _, err := f.m.ProvisionFixedWorker(t.Context(), slot, checkout, remote, []string{"namespace", "boat"})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.m.Get(worker.ID)
	if err != nil || machine.Provider != "boat" || boat.allocations != 1 || namespace.attempts != 0 {
		t.Fatalf("retry changed provider or allocated another slot: %v", err)
	}
}

func TestFixedIdleStopsOnlyPoolWorkersAndWakesSameVM(t *testing.T) {
	f := newIdleFleet(t)
	host := &dispatchProvider{mockProvider: f.p, driver: "mock"}
	f.options.Providers["mock"] = host
	f.options.Providers["manual"] = newProviderWithIdentity(t, "manual-environment", "manual-service-token")
	f.options.IdleFixedOnly = true
	f.restart(t)
	const remote = "https://github.com/octocat/Hello-World.git"
	manual, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "unmanaged-idle", Provider: "manual", Template: provider.Template{Snapshot: "golden"}})
	if err != nil {
		t.Fatal(err)
	}
	slot := strings.Repeat("a", 32)
	fixed, _, err := f.m.ProvisionFixedWorker(t.Context(), slot, strings.Repeat("b", 32), remote, []string{"mock"})
	if err != nil || fixed.Activity == nil {
		t.Fatalf("pool worker missing idle activity gate: %v", err)
	}
	before, err := f.m.Get(fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.setShell(`{"threads":[{"status":"idle","activeRunId":null},{"status":"waiting","activeRunId":null}],"archivedThreads":[]}`)
	f.advance(time.Minute)
	if err := f.m.CheckIdle(t.Context()); err != nil || f.stopCount() != 0 {
		t.Fatalf("idle sweep stopped the shared VM with a waiting thread: %v", err)
	}
	f.setShell(idleShell)
	f.advance(time.Minute)
	done := fixed.Activity.Begin()
	if err := f.m.CheckIdle(t.Context()); err != nil || f.stopCount() != 0 {
		t.Fatalf("idle sweep stopped a worker during useful traffic: %v", err)
	}
	done()
	f.advance(time.Minute)
	if err := f.m.CheckIdle(t.Context()); err != nil || f.stopCount() != 1 || f.m.FixedWorkerState(slot) != "stopped" {
		t.Fatalf("idle sweep did not stop just the pool worker: %v", err)
	}
	stillRunning, err := f.m.Get(manual.ID)
	if err != nil || stillRunning.State != "ready" {
		t.Fatalf("fixed policy stopped a manual worker: %v", err)
	}
	after, err := f.m.LookupFixedWorker(slot)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := after.Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if upstream.Endpoint.Close != nil {
		defer func() { _ = upstream.Endpoint.Close() }()
	}
	resumed, err := f.m.Get(after.ID)
	if err != nil || resumed.ID != before.ID || resumed.RemoteID != before.RemoteID || resumed.State != "ready" ||
		host.starts != 1 || host.allocations != 1 {
		t.Fatalf("wake did not resume the existing pool machine: %v", err)
	}
}
