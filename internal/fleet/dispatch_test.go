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
	driver       string
	scope        string
	placement    provider.Placement
	createErrors []error
	lastRequest  provider.CreateRequest
	onCreate     func()
	attempts     int
	checkouts    int
	bootstrap    int
	getErr       error
	deletes      int
}

func (p *dispatchProvider) Name() string                         { return p.driver }
func (p *dispatchProvider) ScopeID() string                      { return p.scope }
func (p *dispatchProvider) DefaultPlacement() provider.Placement { return p.placement }

func (p *dispatchProvider) Get(ctx context.Context, id string) (provider.Machine, error) {
	if p.getErr != nil {
		return provider.Machine{}, p.getErr
	}
	return p.mockProvider.Get(ctx, id)
}

func (p *dispatchProvider) Delete(ctx context.Context, id string) error {
	p.deletes++
	return p.mockProvider.Delete(ctx, id)
}

type shapedDispatchProvider struct {
	*dispatchProvider
	shape provider.Shape
}

func (p *shapedDispatchProvider) ResolveShape(string) (provider.Shape, error) { return p.shape, nil }

type passiveDispatchProvider struct {
	*dispatchProvider
	passiveDials int
}

func (p *passiveDispatchProvider) ConnectPassive(ctx context.Context, id string, port int) (provider.Endpoint, error) {
	p.passiveDials++
	return p.Connect(ctx, id, port)
}

func TestPassiveDispatchReadNeverStartsStoppedWorker(t *testing.T) {
	f := newFleet(t)
	host := &passiveDispatchProvider{dispatchProvider: &dispatchProvider{mockProvider: f.p, driver: "mock"}}
	f.options.Providers["mock"] = host
	f.restart(t)
	worker, _, err := f.m.ProvisionDispatchWorker(t.Context(), strings.Repeat("a", 32), "https://github.com/octocat/Hello-World.git", []string{"mock"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ResolvePassive(t.Context()); err != nil || host.passiveDials != 1 {
		t.Fatalf("running worker did not use its passive connector: %v", err)
	}
	machine, err := f.m.Get(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The local state still says ready after the provider stops the machine.
	if err := host.Stop(t.Context(), machine.RemoteID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ResolvePassive(t.Context()); err == nil || host.passiveDials != 1 || host.starts != 0 || host.allocations != 1 {
		t.Fatalf("passive read changed a sleeping worker: err=%v, dials=%d, starts=%d", err, host.passiveDials, host.starts)
	}
	if _, err := worker.Resolve(t.Context()); err != nil || host.starts != 1 {
		t.Fatalf("explicit thread request did not wake the selected worker: %v", err)
	}
	if err := f.m.Stop(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ResolvePassive(t.Context()); err == nil || host.passiveDials != 1 || host.starts != 1 {
		t.Fatalf("passive read changed a manually stopped worker: %v", err)
	}
}

func TestNoPassiveConnectorDoesNotPreventExplicitWake(t *testing.T) {
	f, host, _ := dispatchProviders(t)
	worker, _, err := f.m.ProvisionDispatchWorker(t.Context(), strings.Repeat("a", 32), "https://github.com/octocat/Hello-World.git", []string{"boat"})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.m.Get(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Stop(t.Context(), machine.RemoteID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ResolvePassive(t.Context()); !errors.Is(err, provider.ErrUnsupported) || host.starts != 0 {
		t.Fatalf("passive read used a provider without a safe transport: %v", err)
	}
	if _, err := worker.Resolve(t.Context()); err != nil || host.starts != 1 {
		t.Fatalf("explicit request did not wake a just-suspended worker: %v", err)
	}
}

func (p *dispatchProvider) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	p.attempts++
	p.lastRequest = req
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
	boat := &dispatchProvider{mockProvider: f.p, driver: "boat"}
	namespace := &dispatchProvider{mockProvider: newProvider(t), driver: "namespace"}
	f.options.Providers["boat"] = boat
	f.options.Providers["namespace"] = namespace
	f.options.Templates["boat"] = provider.Template{Snapshot: "golden"}
	f.options.Templates["namespace"] = provider.Template{Image: "registered:example-org/t3-worker"}
	f.restart(t)
	return f, boat, namespace
}

func TestDispatchFallbackRecoversSameDevboxAfterRestart(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
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

func TestNamedProvidersKeepTheirOwnPlacementAcrossRestart(t *testing.T) {
	f := newFleet(t)
	primary := &dispatchProvider{mockProvider: f.p, driver: "boat"}
	secondary := &dispatchProvider{mockProvider: newProvider(t), driver: "boat"}
	oldBoat := provider.Placement{Environment: "original"}
	secondary.placement = oldBoat
	oldSite := provider.Placement{Site: "sfo"}
	oldShape := provider.Shape{VirtualCPU: 12, MemoryMB: 24576, Architecture: "arm64"}
	newShape := provider.Shape{VirtualCPU: 4, MemoryMB: 8192, Architecture: "amd64"}
	last := &shapedDispatchProvider{dispatchProvider: &dispatchProvider{mockProvider: newProvider(t), driver: "namespace"}, shape: oldShape}
	last.placement = oldSite
	f.options.Providers = map[string]provider.Provider{"fast": primary, "spare": secondary, "burst": last}
	f.options.Templates = map[string]provider.Template{
		"fast": {Snapshot: "first"}, "spare": {Snapshot: "second"}, "burst": {Image: "registered:org/worker"},
	}
	f.options.Sizes = map[string]string{"fast": "large", "spare": "small"} // burst uses an explicit physical shape
	f.restart(t)
	key := strings.Repeat("a", 32)
	const remote = "https://github.com/octocat/Hello-World.git"
	order := []string{"fast", "spare", "burst"}

	// Cancellation after confirmed refusal leaves an unattempted reservation.
	// Editing configuration before retry must not change the saved launch.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	primary.onCreate = cancel
	primary.createErrors = []error{provider.ErrCreateRejected}
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, key, remote, order); err == nil {
		t.Fatal("expected an interrupted launch")
	}
	primary.onCreate = nil
	last.shape = newShape
	last.placement = provider.Placement{Site: "new-site"}
	primary.placement = provider.Placement{Environment: "do-not-inject"}
	secondary.placement = provider.Placement{Environment: "changed"}
	f.options.Sizes["burst"] = "s"
	f.options.Templates["burst"] = provider.Template{Image: "registered:org/different"}
	f.restart(t)
	var attempts []string
	primary.onCreate = func() { attempts = append(attempts, "fast") }
	secondary.onCreate = func() { attempts = append(attempts, "spare") }
	last.onCreate = func() { attempts = append(attempts, "burst") }
	primary.createErrors = []error{provider.ErrCreateRejected}
	secondary.createErrors = []error{provider.ErrCreateRejected}
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, []string{"burst"}); err != nil {
		t.Fatal(err)
	}
	if primary.lastRequest.Placement == nil || *primary.lastRequest.Placement != (provider.Placement{}) ||
		secondary.lastRequest.Placement == nil || *secondary.lastRequest.Placement != oldBoat ||
		primary.lastRequest.Size != "large" || primary.lastRequest.Template.Snapshot != "first" ||
		secondary.lastRequest.Size != "small" || secondary.lastRequest.Template.Snapshot != "second" {
		t.Fatalf("Boat instances shared settings: primary %+v, secondary %+v", primary.lastRequest, secondary.lastRequest)
	}
	if primary.lastRequest.OperationID == secondary.lastRequest.OperationID || secondary.lastRequest.OperationID == last.lastRequest.OperationID {
		t.Fatal("distinct requests reused a rejected idempotency key")
	}
	if !slices.Equal(attempts, order) || len(last.requests) != 1 || last.requests[0].Size != "" || last.requests[0].Shape != oldShape ||
		last.requests[0].Placement == nil || *last.requests[0].Placement != oldSite ||
		last.requests[0].Template.Image != "registered:org/worker" || last.bootstrap != 1 {
		t.Fatalf("saved placement lost: attempts %v, requests %+v, bootstraps %d", attempts, last.requests, last.bootstrap)
	}
	batch, err := f.m.Batch(key)
	if err != nil || len(batch.Machines) != 1 || batch.Machines[0].Provider != "burst" {
		t.Fatalf("saved provider instance not visible: %+v, %v", batch, err)
	}
	f.restart(t)
	if err := f.m.ReleaseDispatchWorker(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	// A fresh reservation picks up the new configuration.
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), strings.Repeat("b", 32), remote, []string{"burst"}); err != nil {
		t.Fatal(err)
	}
	if len(last.requests) != 2 || last.requests[1].Size != "s" || last.requests[1].Shape != newShape || last.requests[1].Template.Image != "registered:org/different" || last.requests[1].Placement == nil || *last.requests[1].Placement != last.placement {
		t.Fatalf("fresh reservation used stale settings: %+v", last.requests)
	}
}

func TestDispatchRefusesReboundProviderInstance(t *testing.T) {
	f := newFleet(t)
	f.options.Providers["primary"] = &dispatchProvider{mockProvider: f.p, driver: "boat", scope: "account-a"}
	f.options.Templates["primary"] = provider.Template{Snapshot: "golden"}
	f.restart(t)
	key := strings.Repeat("c", 32)
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, "https://github.com/octocat/Hello-World.git", []string{"primary"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	replacement := &dispatchProvider{mockProvider: newProvider(t), driver: "namespace"}
	f.options.Providers["primary"] = replacement
	if _, err := fleet.New(f.options); err == nil || !strings.Contains(err.Error(), "requires provider") || replacement.attempts != 0 {
		t.Fatalf("startup did not reject a reassigned provider: %v", err)
	}
	replacement.driver, replacement.scope = "boat", "account-b"
	if _, err := fleet.New(f.options); err == nil || !strings.Contains(err.Error(), "original account") || replacement.attempts != 0 {
		t.Fatalf("startup did not reject a reassigned account: %v", err)
	}
}

func TestFallbackRefusesReboundAccount(t *testing.T) {
	f, primary, backup := dispatchProviders(t)
	backup.scope = "account-a"
	order := []string{"boat", "namespace"}
	key := strings.Repeat("d", 32)
	const remote = "https://github.com/octocat/Hello-World.git"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	primary.onCreate = cancel
	primary.createErrors = []error{provider.ErrCreateRejected}
	if _, _, err := f.m.ProvisionDispatchWorker(ctx, key, remote, order); err == nil {
		t.Fatal("expected cancelled launch after reservation")
	}
	primary.onCreate = nil
	primary.createErrors = []error{provider.ErrCreateRejected}
	backup.scope = "account-b"
	f.restart(t)
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, order); err == nil || backup.attempts != 0 {
		t.Fatalf("retried a fallback in a different account: %v", err)
	}
	backup.scope = "account-a"
	primary.createErrors = []error{provider.ErrCreateRejected}
	f.restart(t)
	if _, _, err := f.m.ProvisionDispatchWorker(t.Context(), key, remote, order); err != nil || backup.attempts != 1 {
		t.Fatalf("restoring fallback account did not recover reservation: %v", err)
	}
}

func TestDispatchTriesFallbacksInOrder(t *testing.T) {
	f, boat, namespace := dispatchProviders(t)
	var calls []string
	boat.onCreate = func() { calls = append(calls, "boat") }
	namespace.onCreate = func() { calls = append(calls, "namespace") }
	third := &dispatchProvider{mockProvider: newProvider(t), driver: "boat", onCreate: func() { calls = append(calls, "third") }}
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
	var old map[string]any
	if err := f.store.Load(&old); err != nil {
		t.Fatal(err)
	}
	old["schema"] = 1
	for _, entry := range old["machines"].([]any) {
		delete(entry.(map[string]any), "driver")
	}
	if err := f.store.Save(old); err != nil {
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
	boat.getErr = errors.New("provider unavailable")
	if lost, err := f.m.DispatchWorkerLost(ctx, key); lost || err == nil {
		t.Fatalf("an uncertain worker was reported lost: %v %v", lost, err)
	}
	boat.getErr = nil
	if err := f.m.Stop(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if lost, err := f.m.DispatchWorkerLost(ctx, key); lost || err != nil {
		t.Fatalf("a stopped worker was reported lost: %v %v", lost, err)
	}
	// Removed outside t3fleet, for example from the provider console.
	if err := boat.mockProvider.Delete(ctx, worker.RemoteID); err != nil {
		t.Fatal(err)
	}
	if lost, err := f.m.DispatchWorkerLost(ctx, key); !lost || err != nil {
		t.Fatalf("a removed worker was not reported lost: %v %v", lost, err)
	}
	if _, err := f.m.Get(worker.ID); !errors.Is(err, fleet.ErrNotFound) || boat.deletes != 0 {
		t.Fatalf("loss check did not retire only local state: err=%v, provider deletes=%d", err, boat.deletes)
	}
	f.restart(t)
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
