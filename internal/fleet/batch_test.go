package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/flemzord/t3fleet/pkg/provider"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/fleet"
)

func batchRequest() fleet.BatchRequest {
	return fleet.BatchRequest{ID: strings.Repeat("a", 32), CreateRequest: fleet.CreateRequest{Name: "workers", Provider: "mock", Size: "large"}, Count: 10}
}

func TestBatchDurablePlanPartialFailureAndReplay(t *testing.T) {
	f := newFleet(t)
	useIsolatedRuntime(t, f)
	req := batchRequest()
	batch, err := f.m.PrepareBatch(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.p.allocations != 0 || len(batch.Machines) != 10 {
		t.Fatal("prepare allocated cloud resources")
	}
	seen := map[string]bool{}
	for _, m := range batch.Machines {
		if seen[m.ID] || m.State != "planned" {
			t.Fatal("nonunique intent")
		}
		seen[m.ID] = true
	}
	f.p.createFailsOnce = true
	if err := f.m.Start(t.Context(), batch.Machines[0].ID); err == nil {
		t.Fatal("expected ambiguous failure")
	}
	if err := f.m.Start(t.Context(), batch.Machines[1].ID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	resumed, err := f.m.Batch(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range resumed.Machines {
		if m.ID != batch.Machines[i].ID {
			t.Fatal("lost operation identity")
		}
	}
	replay, err := f.m.PrepareBatch(t.Context(), req)
	if err != nil || replay.Machines[0].ID != batch.Machines[0].ID {
		t.Fatal("unsafe replay", err)
	}
	if err := f.m.Start(t.Context(), batch.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	if f.p.allocations != 2 {
		t.Fatalf("recovery duplicated cloud allocation: %d", f.p.allocations)
	}
	for _, r := range f.p.requests {
		if r.Size != "large" || r.Template.Snapshot != "golden" {
			t.Fatal("batch settings diverged")
		}
	}
	req.Count = 9
	if _, err := f.m.PrepareBatch(t.Context(), req); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("reused batch accepted different request")
	}
	if err := f.m.Delete(t.Context(), batch.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	req.Count = 10
	replay, err = f.m.PrepareBatch(t.Context(), req)
	if err != nil || replay.Machines[0].State != "deleted" || len(f.m.List()) != 9 {
		t.Fatal("replay resurrected deleted member")
	}
}

func TestBatchFullNamePreflightAndCancellation(t *testing.T) {
	f := newFleet(t)
	req := batchRequest()
	if _, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "workers-10", Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.PrepareBatch(t.Context(), req); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal(err)
	}
	if len(f.m.List()) != 1 || f.p.allocations != 1 {
		t.Fatal("partial batch reserved despite late collision")
	}
	req.Name = strings.Repeat("n", 31)
	if _, err := f.m.PrepareBatch(t.Context(), req); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("accepted overlong suffix")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req.Name = "cancelled"
	if _, err := f.m.PrepareBatch(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(f.m.List()) != 1 {
		t.Fatal("cancelled preparation changed state")
	}
}

// Each mock runtime returns a separate identity and service token, as required
// of a credential-free golden template. Connections reject cross-machine tokens.
type isolatedProvider struct {
	*mockProvider
	endpoint string
}

func (p *isolatedProvider) Exec(ctx context.Context, id string, _ provider.ExecRequest) (provider.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.ExecResult{}, err
	}
	data, _ := json.Marshal(map[string]string{"token": "service-" + id, "environmentId": "env-" + id, "serverVersion": serverVersion})
	return provider.ExecResult{Stdout: string(data)}, nil
}
func (p *isolatedProvider) Connect(ctx context.Context, id string, _ int) (provider.Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return provider.Endpoint{}, err
	}
	return provider.Endpoint{URL: p.endpoint + "?machine=" + id}, nil
}
func useIsolatedRuntime(t *testing.T, f *fixture) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("machine")
		if id == "" || r.Header.Get("Authorization") != "Bearer service-"+id {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/.well-known/t3/environment" {
			json.NewEncoder(w).Encode(map[string]any{"environmentId": "env-" + id, "serverVersion": serverVersion})
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	f.options.Providers["mock"] = &isolatedProvider{mockProvider: f.p, endpoint: upstream.URL}
	f.restart(t)
}

func TestBatchIndependentRuntimeAndClientAuthorization(t *testing.T) {
	f := newFleet(t)
	useIsolatedRuntime(t, f)
	req := batchRequest()
	req.Count = 2
	plan, err := f.m.PrepareBatch(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 2)
	for i, m := range plan.Machines {
		if err := f.m.Start(t.Context(), m.ID); err != nil {
			t.Fatal(err)
		}
		tokens[i] = pair(t, f, m)
	}
	if tokens[0] == tokens[1] {
		t.Fatal("shared client credential")
	}
	f.restart(t)
	for i, m := range plan.Machines {
		if status, _ := request(t, m.URL+"/api/probe", tokens[i]); status != 200 {
			t.Fatal("own credential rejected", status)
		}
		if status, _ := request(t, m.URL+"/api/probe", tokens[1-i]); status != 401 {
			t.Fatal("foreign credential accepted", status)
		}
		status, descriptor := request(t, m.URL+"/.well-known/t3/environment", "")
		current, _ := f.m.Get(m.ID)
		if status != 200 || !strings.Contains(string(descriptor), "env-"+current.RemoteID) {
			t.Fatal("runtime identity mismatch")
		}
	}
}

type failingBatchStore struct{ fleet.Storage }

func (failingBatchStore) Save(any) error { return errors.New("disk unavailable") }
func TestBatchPersistenceFailureDoesNotAllocate(t *testing.T) {
	f := newFleet(t)
	f.options.Store = failingBatchStore{f.store}
	f.m.Close(t.Context())
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.PrepareBatch(t.Context(), batchRequest()); err == nil {
		t.Fatal("failed persistence accepted")
	}
	if len(f.m.List()) != 0 || f.p.allocations != 0 {
		t.Fatal("failed intent left allocations")
	}
}

func TestDeleteUnallocatedBatchIntentWithoutCloudCall(t *testing.T) {
	f := newFleet(t)
	req := batchRequest()
	req.Count = 1
	plan, err := f.m.PrepareBatch(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(t.Context(), plan.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	saved, err := f.m.Batch(req.ID)
	if err != nil || saved.Machines[0].State != "deleted" || f.p.allocations != 0 {
		t.Fatal("deleted plan was allocated or resurrected")
	}
}

func TestBatchRequiresPinnedSourceSnapshot(t *testing.T) {
	f := newFleet(t)
	req := batchRequest()
	req.Template = provider.Template{Source: "golden"}
	if _, err := f.m.PrepareBatch(t.Context(), req); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("batch accepted latest source snapshot")
	}
	if len(f.m.List()) != 0 || f.p.allocations != 0 {
		t.Fatal("invalid template reserved resources")
	}
}

func TestBatchConcurrentStartAndDeleteNeverOrphanAllocation(t *testing.T) {
	f := newFleet(t)
	for i := range 30 {
		req := batchRequest()
		req.ID = fmt.Sprintf("%032x", i+1)
		req.Name = fmt.Sprintf("race-%d", i)
		req.Count = 1
		plan, err := f.m.PrepareBatch(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		id := plan.Machines[0].ID
		gate := make(chan struct{})
		done := make(chan error, 2)
		go func() { <-gate; done <- f.m.Start(t.Context(), id) }()
		go func() { <-gate; done <- f.m.Delete(t.Context(), id) }()
		close(gate)
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, fleet.ErrNotFound) {
				t.Fatal(err)
			}
		}
		f.p.mu.Lock()
		remaining := len(f.p.machines)
		f.p.mu.Unlock()
		if remaining != 0 || len(f.m.List()) != 0 {
			t.Fatal("concurrent deletion left orphaned allocation")
		}
	}
}

func TestBatchRejectsSharedRuntimeIdentity(t *testing.T) {
	f := newFleet(t)
	req := batchRequest()
	req.Count = 2
	plan, err := f.m.PrepareBatch(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Start(t.Context(), plan.Machines[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Start(t.Context(), plan.Machines[1].ID); err == nil {
		t.Fatal("cloned runtime credentials accepted")
	}
	second, _ := f.m.Get(plan.Machines[1].ID)
	if second.State != "not-ready" || second.RemoteID == "" {
		t.Fatal("shared runtime failure lost recoverable identity")
	}
}
