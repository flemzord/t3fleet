package namespace

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserveStatesUsesExactIDsAndIsolatesFailedReads(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != servicePath+"Fetch" || r.Header.Get("Authorization") != "Bearer "+fixtureToken(testTenant, "tester") {
			t.Errorf("unexpected or unauthenticated RPC: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		in := body(t, r)
		id, _ := in["id"].(string)
		if len(in) != 2 || id == "" || in["returnActivatedInstance"] != true {
			t.Errorf("Fetch must request only the exact ID and active instance: %v", in)
		}
		mu.Lock()
		calls[id]++
		mu.Unlock()
		box := map[string]string{"id": id, "name": "worker"}
		result := map[string]any{"devbox": box}
		switch id {
		case "up":
			result["instanceId"] = "active-instance"
		case "asleep":
			// An intact devbox without an activated instance is stopped.
		case "expired":
			box["destroyedAt"] = "2026-11-19T09:00:00Z"
			result["instanceId"] = "old-instance"
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			return
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "malformed":
			_, _ = io.WriteString(w, "{")
			return
		case "incomplete":
			delete(result, "devbox")
		case "wrong-id":
			box["id"] = "another-devbox"
		}
		reply(t, w, result)
	}, nil)

	ids := []string{"up", "asleep", "expired", "missing", "unavailable", "malformed", "incomplete", "wrong-id"}
	states, err := client.ObserveStates(t.Context(), append(ids, "", "up"))
	if err != nil || !maps.Equal(states, map[string]string{"up": "running", "asleep": "stopped", "expired": "deleted"}) {
		t.Fatalf("observed %v, error %v", states, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != len(ids) {
		t.Fatalf("unexpected requests: %v", calls)
	}
	for _, id := range ids {
		if calls[id] != 1 {
			t.Errorf("expected one Fetch for %q, got %d", id, calls[id])
		}
	}
}

func TestObserveStatesNeverContactsAnotherTenant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("credential for another tenant reached the control plane")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := New(Config{TenantID: "other-tenant", Endpoint: server.URL, TokenSource: testToken{}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.ObserveStates(t.Context(), []string{"", ""}); err != nil || len(got) != 0 {
		t.Fatalf("empty inventory should not need credentials: %v, error %v", got, err)
	}
	if got, err := client.ObserveStates(t.Context(), []string{"managed"}); err == nil || len(got) != 0 {
		t.Fatalf("foreign tenant produced observations: %v, error %v", got, err)
	}
}

func TestObserveStatesBoundsRequestsAndKeepsCompletedReadsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var active, calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > 4 {
			t.Errorf("more than four concurrent Fetch requests: %d", n)
		}
		id, _ := body(t, r)["id"].(string)
		order := calls.Add(1)
		switch {
		case order <= 4:
			reply(t, w, map[string]any{"devbox": map[string]string{"id": id, "name": "worker"}})
			return
		case order == 5:
			// At least one earlier read has completed to free a worker.
			cancel()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("in-flight Fetch ignored cancellation")
		}
	}, nil)
	ids := make([]string, 30)
	for i := range ids {
		ids[i] = fmt.Sprintf("remote-%d", i)
	}
	states, err := client.ObserveStates(ctx, ids)
	if err != nil || len(states) == 0 || len(states) > 4 {
		t.Fatalf("expected only completed reads, got %v, error %v", states, err)
	}
	for _, state := range states {
		if state != "stopped" {
			t.Fatalf("invented a state for an unfinished fetch: %v", states)
		}
	}
	before := calls.Load()
	got, _ := client.ObserveStates(ctx, ids)
	if len(got) != 0 || calls.Load() != before {
		t.Fatalf("already cancelled observation submitted a request: results=%v", got)
	}
}
