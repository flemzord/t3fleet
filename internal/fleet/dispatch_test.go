package fleet_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type dispatchProvider struct {
	*mockProvider
	createErrors []error
	onCreate     func()
	attempts     int
	checkouts    int
	bootstrap    int
	lastSize     string
}

func (p *dispatchProvider) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	p.attempts++
	p.lastSize = req.Size
	if p.onCreate != nil {
		p.onCreate()
	}
	if len(p.createErrors) > 0 {
		err := p.createErrors[0]
		p.createErrors = p.createErrors[1:]
		if err != nil {
			return provider.Machine{}, err
		}
	}
	return p.mockProvider.Create(ctx, req)
}

func (p *dispatchProvider) Exec(ctx context.Context, id string, req provider.ExecRequest) (provider.ExecResult, error) {
	if len(req.Args) >= 4 {
		switch req.Args[3] {
		case "t3fleet-dispatch":
			p.checkouts++
			return provider.ExecResult{Stdout: fmt.Sprintf("/home/devbox/.t3fleet/dispatch-workspaces/%s/repo\n", req.Args[4])}, nil
		case "t3fleet-runtime":
			p.bootstrap++
			req.Args = []string{"/bin/bash", "-lc", "exec t3fleet-runtime-info"}
		}
	}
	return p.mockProvider.Exec(ctx, id, req)
}

func dispatchProviders(t *testing.T) (*fixture, *dispatchProvider, *dispatchProvider) {
	t.Helper()
	f := newFleet(t)
	boat := &dispatchProvider{mockProvider: f.p}
	namespace := &dispatchProvider{mockProvider: newProvider(t)}
	f.options.Providers["boat"] = boat
	f.options.Providers["namespace"] = namespace
	f.options.Templates["boat"] = provider.Template{Snapshot: "golden"}
	f.options.Templates["namespace"] = provider.Template{Image: "registered:example-org/t3-worker"}
	f.restart(t)
	return f, boat, namespace
}

func TestDispatchFallbackRecoversSameDevboxAfterRestart(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	f.options.DefaultSizes = map[string]string{"boat": "small", "namespace": "m"}
	f.restart(t)
	boat.createErrors = []error{provider.ErrCreateRejected}
	namespace.createFailsOnce = true // Namespace allocates, then loses its reply.
	key := strings.Repeat("a", 32)
	remote := "https://github.com/octocat/Hello-World.git"
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err == nil {
		t.Fatal("expected interrupted Namespace create")
	}
	first, err := f.m.Batch(key)
	if err != nil || len(first.Machines) != 1 || first.Machines[0].Provider != "namespace" {
		t.Fatalf("fallback reservation was not saved: %+v, %v", first, err)
	}
	if boat.lastSize != "small" || namespace.lastSize != "m" {
		t.Fatalf("provider sizes: Boat=%q Namespace=%q", boat.lastSize, namespace.lastSize)
	}
	f.options.DefaultSizes = map[string]string{"boat": "large", "namespace": "l"}
	f.restart(t)
	// Changing the policy does not reroute an existing reservation.
	if _, cwd, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"namespace", "boat"}); err != nil || cwd == "" {
		t.Fatalf("Namespace worker could not recover through restart: %q, %v", cwd, err)
	}
	batch, _ := f.m.Batch(key)
	if len(batch.Machines) != 1 || batch.Machines[0].ID != first.Machines[0].ID || batch.Machines[0].State != "ready" {
		t.Fatalf("fallback lost its original machine: %+v", batch)
	}
	if boat.attempts != 1 || boat.allocations != 0 || namespace.allocations != 1 || namespace.bootstrap != 1 ||
		namespace.checkouts != 1 || namespace.requests[1].Template.Image != "registered:example-org/t3-worker" {
		t.Fatalf("unexpected provider activity: Boat attempts=%d, Namespace allocations=%d", boat.attempts, namespace.allocations)
	}
	if namespace.lastSize != "m" {
		t.Fatalf("fallback size was not preserved after restart: %q", namespace.lastSize)
	}
	if _, err := f.m.LookupDispatchWorker(key); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(t.Context(), batch.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err == nil || namespace.allocations != 1 {
		t.Fatal("retry resurrected a deleted worker")
	}
}

func TestDispatchDoesNotSwitchAfterUncertainBoatCreate(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	boat.createErrors = []error{errors.New("response lost"), provider.ErrCreateRejected}
	key := strings.Repeat("b", 32)
	for range 2 {
		if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, "https://github.com/octocat/Hello-World.git", []string{"boat", "namespace"}); err == nil {
			t.Fatal("expected Boat create failure")
		}
		f.restart(t)
	}
	if boat.attempts != 2 || namespace.attempts != 0 {
		t.Fatalf("ambiguous Boat operation changed providers: Boat=%d Namespace=%d", boat.attempts, namespace.attempts)
	}
}

func TestDispatchRemainsOnBoatAfterAllocation(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	boat.execFails = true
	key := strings.Repeat("c", 32)
	remote := "https://github.com/octocat/Hello-World.git"
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err == nil {
		t.Fatal("expected Boat runtime failure")
	}
	boat.execFails = false
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err != nil {
		t.Fatal(err)
	}
	if boat.allocations != 1 || boat.checkouts != 1 || namespace.attempts != 0 {
		t.Fatal("launch did not stay with its original Boat sandbox")
	}
}

func TestDispatchHonorsNamespaceAsPrimary(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	key := strings.Repeat("d", 32)
	remote := "https://github.com/octocat/Hello-World.git"
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"namespace", "boat"}); err != nil {
		t.Fatal(err)
	}
	if boat.attempts != 0 || namespace.allocations != 1 || namespace.bootstrap != 1 {
		t.Fatal("did not reserve Namespace as the primary worker")
	}
}

func TestDispatchTriesFallbacksInOrder(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	var calls []string
	boat.onCreate = func() { calls = append(calls, "boat") }
	namespace.onCreate = func() { calls = append(calls, "namespace") }
	third := &dispatchProvider{mockProvider: newProvider(t), onCreate: func() { calls = append(calls, "third") }}
	f.options.Providers["third"] = third
	f.options.Templates["third"] = provider.Template{Image: "third-image"}
	f.restart(t)
	// These providers must explicitly refuse before the third can allocate.
	// Their counters also catch a retry of an earlier preference.
	boat.createErrors = []error{provider.ErrCreateRejected}
	namespace.createErrors = []error{provider.ErrCreateRejected}
	key := strings.Repeat("e", 32)
	remote := "https://github.com/octocat/Hello-World.git"
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"namespace", "boat", "third"}); err != nil {
		t.Fatal(err)
	}
	batch, err := f.m.Batch(key)
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].Provider != "third" ||
		!slices.Equal(calls, []string{"namespace", "boat", "third"}) || third.allocations != 1 {
		t.Fatalf("unexpected fallback reservation: calls=%v, batch=%v, err=%v", calls, batch, err)
	}
}

func TestDispatchUpgradesUnattemptedLegacyReservation(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	boat.createErrors = []error{provider.ErrCreateRejected}
	key := strings.Repeat("f", 32)
	// The old dispatcher saved a normal, single-machine Boat batch.
	legacy, err := f.m.PrepareBatch(t.Context(), fleet.BatchRequest{
		ID: key, Count: 1, CreateRequest: fleet.CreateRequest{Name: "thread-" + key[:12], Provider: "boat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, "https://github.com/octocat/Hello-World.git", []string{"boat", "namespace"}); err != nil {
		t.Fatal(err)
	}
	current, err := f.m.Batch(key)
	if err != nil || current.Machines[0].ID != legacy.Machines[0].ID || namespace.allocations != 1 {
		t.Fatalf("legacy planned worker did not retain its fallback: %v, %v", current, err)
	}
}

func TestReleaseDispatchWorkerOnBothProviders(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"boat", "namespace"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, boat, ns := dispatchProviders(t)
			host := boat
			if name == "namespace" {
				boat.createErrors = []error{provider.ErrCreateRejected}
				host = ns
			}
			key := strings.Repeat("a", 32)
			remote := "https://github.com/octocat/Hello-World.git"
			if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err != nil {
				t.Fatal(err)
			}
			batch, err := f.m.Batch(key)
			if err != nil || len(batch.Machines) != 1 || batch.Machines[0].Provider != name {
				t.Fatalf("worker not in expected provider: %+v %v", batch, err)
			}
			worker := batch.Machines[0]
			host.deleteFails = true
			if err := f.m.ReleaseDispatchWorker(t.Context(), key); err == nil {
				t.Fatal("unconfirmed delete was treated as success")
			}
			if _, err := host.Get(t.Context(), worker.RemoteID); err != nil {
				t.Fatalf("provider lost the worker despite failing deletion: %v", err)
			}
			f.restart(t)
			host.deleteFails = false
			if err := f.m.ReleaseDispatchWorker(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Get(t.Context(), worker.RemoteID); !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("provider still has the archived worker: %v", err)
			}
			f.restart(t)
			if err := f.m.ReleaseDispatchWorker(t.Context(), key); err != nil {
				t.Fatalf("delete replay failed: %v", err)
			}
			if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"boat", "namespace"}); err == nil || host.allocations != 1 {
				t.Fatal("archived worker could be resurrected")
			}
		})
	}
}

func TestReleaseDispatchWorkerReconcilesBoatSandboxAlreadyAbsent(t *testing.T) {
	t.Parallel()
	f, boat, _ := dispatchProviders(t)
	key := strings.Repeat("d", 32)
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, "https://github.com/octocat/Hello-World.git", []string{"boat"}); err != nil {
		t.Fatal(err)
	}
	batch, err := f.m.Batch(key)
	if err != nil {
		t.Fatal(err)
	}
	worker := batch.Machines[0]
	// Boat removed the sandbox, but its asynchronous deletion request
	// could still fail or stall if reissued.
	if err := boat.Delete(t.Context(), worker.RemoteID); err != nil {
		t.Fatal(err)
	}
	boat.deleteFails = true
	f.restart(t)
	if err := f.m.ReleaseDispatchWorker(t.Context(), key); err != nil {
		t.Fatalf("confirmed absent sandbox retained its reservation: %v", err)
	}
	batch, err = f.m.Batch(key)
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].State != "deleted" {
		t.Fatalf("local dispatch tombstone missing: %+v %v", batch, err)
	}
}

func TestReclaimDispatchWorkerDistinguishesNeverAllocatedFromUnknown(t *testing.T) {
	t.Parallel()
	f, boat, _ := dispatchProviders(t)
	ctx := t.Context()
	neverPlanned := strings.Repeat("a", 32)
	if err := f.m.ReclaimDispatchWorker(ctx, neverPlanned); err != nil {
		t.Fatalf("a route without a fleet batch retained capacity: %v", err)
	}

	const remote = "https://github.com/octocat/Hello-World.git"
	rejected := strings.Repeat("b", 32)
	boat.createErrors = []error{provider.ErrCreateRejected}
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, rejected, remote, []string{"boat"}); err == nil {
		t.Fatal("expected the provider to refuse allocation")
	}
	if err := f.m.ReclaimDispatchWorker(ctx, rejected); err != nil {
		t.Fatalf("definitively rejected allocation could not be reclaimed: %v", err)
	}

	unknown := strings.Repeat("c", 32)
	boat.createFailsOnce = true // The provider allocates but loses the reply.
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, unknown, remote, []string{"boat"}); err == nil {
		t.Fatal("expected lost create response")
	}
	boat.createErrors = []error{provider.ErrCreateRejected}
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, unknown, remote, []string{"boat"}); err == nil {
		t.Fatal("expected the provider to refuse the retry")
	}
	if err := f.m.ReclaimDispatchWorker(ctx, unknown); err == nil {
		t.Fatal("a later rejection masked an earlier unknown allocation")
	}
	batch, err := f.m.Batch(unknown)
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].State == "deleted" || boat.allocations != 1 || boat.attempts != 3 {
		t.Fatalf("reclaim unexpectedly removed or retried the ambiguous allocation: %+v, %v", batch, err)
	}
}

func TestDispatchWorkerLostRequiresConfirmedAbsence(t *testing.T) {
	t.Parallel()
	f, boat, _ := dispatchProviders(t)
	ctx := t.Context()
	if lost, err := f.m.DispatchWorkerLost(ctx, strings.Repeat("f", 32)); lost || err != nil {
		t.Fatalf("an unknown reservation was reported lost: %v %v", lost, err)
	}
	key := strings.Repeat("e", 32)
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, key, "https://github.com/octocat/Hello-World.git", []string{"boat"}); err != nil {
		t.Fatal(err)
	}
	batch, err := f.m.Batch(key)
	if err != nil {
		t.Fatal(err)
	}
	worker := batch.Machines[0]
	if lost, err := f.m.DispatchWorkerLost(ctx, key); lost || err != nil {
		t.Fatalf("a running worker was reported lost: %v %v", lost, err)
	}
	if err := f.m.Stop(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if lost, err := f.m.DispatchWorkerLost(ctx, key); lost || err != nil {
		t.Fatalf("a stopped worker was reported lost: %v %v", lost, err)
	}
	// Removed outside t3fleet, for example from the provider console.
	if err := boat.Delete(ctx, worker.RemoteID); err != nil {
		t.Fatal(err)
	}
	if lost, err := f.m.DispatchWorkerLost(ctx, key); !lost || err != nil {
		t.Fatalf("a removed worker was not reported lost: %v %v", lost, err)
	}
	boat.deleteFails = true
	if err := f.m.ReleaseDispatchWorker(ctx, key); err != nil {
		t.Fatalf("a lost worker could not be released: %v", err)
	}
	if lost, err := f.m.DispatchWorkerLost(ctx, key); !lost || err != nil {
		t.Fatalf("a released worker was not reported lost: %v %v", lost, err)
	}
	if allocations := boat.allocations; allocations != 1 {
		t.Fatalf("checking a lost worker allocated a machine: %d", allocations)
	}
}
