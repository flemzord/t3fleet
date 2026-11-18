package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

func TestBatchBoundedConcurrencyPartialResultsAndAssociation(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	token, err := ensureAdminToken(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	plan := fleet.Batch{ID: strings.Repeat("a", 32)}
	for i := range 10 {
		plan.Machines = append(plan.Machines, fleet.Machine{ID: fmt.Sprintf("id-%d", i), Name: fmt.Sprintf("worker-%02d", i+1), State: "creating"})
	}
	var mu sync.Mutex
	active, peak, starts, pairs, opens := 0, 0, 0, 0, 0
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing admin auth")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/batches":
			_ = json.NewEncoder(w).Encode(plan)
		case strings.HasSuffix(r.URL.Path, "/start"):
			mu.Lock()
			active++
			starts++
			if active > peak {
				peak = active
			}
			if starts == 3 {
				close(gate)
			}
			mu.Unlock()
			select {
			case <-gate:
			case <-time.After(3 * time.Second):
				t.Error("workers did not run concurrently")
			}
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			if strings.Contains(r.URL.Path, "id-4/") {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"error":"operation_failed"}`)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasSuffix(r.URL.Path, "/pair"):
			mu.Lock()
			pairs++
			mu.Unlock()
			fmt.Fprint(w, `{"url":"http://127.0.0.1:47777/pair#token=secret"}`)
		case r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/v1/machines/")
			state := "ready"
			if id == "id-4" {
				state = "create-failed"
			}
			_ = json.NewEncoder(w).Encode(fleet.Machine{ID: id, State: state})
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	old := openT3
	defer func() { openT3 = old }()
	openT3 = func(context.Context, string) error { opens++; return nil }
	var out, diag bytes.Buffer
	err = runBatchWithInput(t.Context(), cfg, fleet.BatchRequest{ID: plan.ID, Count: 10, CreateRequest: fleet.CreateRequest{Name: "worker", Provider: "mock"}}, 3, false, true, true, strings.NewReader(strings.Repeat("\n", 10)), true, &out, &diag)
	if err == nil {
		t.Fatal("partial failure returned success")
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 10 || peak != 3 || pairs != 9 || opens != 9 {
		t.Fatalf("starts=%d peak=%d pairs=%d opens=%d", starts, peak, pairs, opens)
	}
	var result struct {
		Results []batchResult `json:"results"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || len(result.Results) != 10 || result.Results[4].Error != "start_failed" {
		t.Fatal("missing individual results", out.String())
	}
	if strings.Contains(diag.String(), "token=") || strings.Contains(out.String(), "secret") {
		t.Fatal("credential leaked")
	}
}

func TestBatchCancellationNeverRegistersAndReportsEveryIntent(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mustAdminToken(t, cfg.StateDir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	plan := fleet.Batch{ID: strings.Repeat("b", 32), Machines: []fleet.Machine{{ID: "one", State: "creating"}, {ID: "two", State: "creating"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/batches" {
			_ = json.NewEncoder(w).Encode(plan)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/start") {
			cancel()
			<-r.Context().Done()
			return
		}
		t.Error("unexpected request after cancellation")
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out, diag bytes.Buffer
	if err := runBatchWithInput(ctx, cfg, fleet.BatchRequest{ID: plan.ID, Count: 2}, 1, false, true, true, strings.NewReader(""), true, &out, &diag); err == nil {
		t.Fatal("cancelled operation succeeded")
	}
	var result struct {
		Results []batchResult `json:"results"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || len(result.Results) != 2 {
		t.Fatal("lost recovery IDs")
	}
}

func TestBatchCLIHeadlessAndResumeSkipReadyOrDeleted(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mustAdminToken(t, cfg.StateDir)
	plan := fleet.Batch{ID: strings.Repeat("c", 32), Machines: []fleet.Machine{{ID: "one", State: "planned"}, {ID: "two", State: "planned"}}}
	starts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/batches":
			var req fleet.BatchRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Count != 2 || req.Name != "workers" || req.Size != "large" || req.Template.Snapshot != "template" {
				t.Error("CLI flags lost")
			}
			_ = json.NewEncoder(w).Encode(plan)
		case r.URL.Path == "/v1/batches/"+plan.ID:
			_ = json.NewEncoder(w).Encode(fleet.Batch{ID: plan.ID, Machines: []fleet.Machine{{ID: "one", State: "ready"}, {ID: "two", State: "deleted"}}})
		case strings.HasSuffix(r.URL.Path, "/start"):
			starts++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/pair"):
			t.Error("headless command paired machine")
		default:
			_ = json.NewEncoder(w).Encode(fleet.Machine{ID: strings.TrimPrefix(r.URL.Path, "/v1/machines/"), State: "ready"})
		}
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out, diag bytes.Buffer
	if err := runClient(t.Context(), cfg, "create", []string{"--name", "workers", "--provider", "mock", "--size", "large", "--snapshot", "template", "--count", "2", "--concurrency", "1", "--batch-id", plan.ID, "--no-register", "--json"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if err := runClient(t.Context(), cfg, "create", []string{"--resume", plan.ID, "--no-register"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if starts != 2 {
		t.Fatal("resume restarted ready/deleted members", starts)
	}
}

func TestBatchAssociationCancellationDefersOtherLinks(t *testing.T) {
	for _, input := range []string{"q\n", ""} {
		t.Run(fmt.Sprintf("input-%q", input), func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = t.TempDir()
			mustAdminToken(t, cfg.StateDir)
			plan := fleet.Batch{ID: strings.Repeat("d", 32), Machines: []fleet.Machine{{ID: "one", State: "ready"}, {ID: "two", State: "ready"}}}
			pairs := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1/batches"):
					_ = json.NewEncoder(w).Encode(plan)
				case strings.HasSuffix(r.URL.Path, "/pair"):
					pairs++
					fmt.Fprint(w, `{"url":"http://127.0.0.1:47777/pair#token=secret"}`)
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(fleet.Machine{ID: strings.TrimPrefix(r.URL.Path, "/v1/machines/"), State: "ready"})
				default:
					t.Error("unexpected cloud mutation")
				}
			}))
			defer server.Close()
			cfg.Listen = strings.TrimPrefix(server.URL, "http://")
			previous := openT3
			defer func() { openT3 = previous }()
			openT3 = func(context.Context, string) error { return nil }
			var out, diag bytes.Buffer
			err := runBatchWithInput(t.Context(), cfg, fleet.BatchRequest{ID: plan.ID}, 1, true, true, true, strings.NewReader(input), true, &out, &diag)
			if err == nil || pairs != 1 || !strings.Contains(out.String(), `"association":"pending"`) {
				t.Fatal("ignored association cancellation", err, pairs, out.String())
			}
		})
	}
}
