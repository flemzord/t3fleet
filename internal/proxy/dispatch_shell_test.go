package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestShellKeepsProjectsAndStoppedThreadsWithoutWakingWorkers(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	const project = "project"
	thread := func(id, title string) map[string]any {
		return map[string]any{"id": id, "projectId": project, "title": title, "latestVisibleMessage": map[string]string{"text": "private reply"}}
	}
	snapshot := func(projects, threads, archived []any) map[string]any {
		return map[string]any{"schemaVersion": 1, "snapshotSequence": 123, "projects": projects, "threads": threads, "archivedThreads": archived}
	}
	resolve := func(data any) func(context.Context) (Upstream, error) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != dispatchShellPath {
				t.Errorf("unexpected request to %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(data)
		}))
		t.Cleanup(s.Close)
		return func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: s.URL}, Token: "local"}, nil
		}
	}
	var active, passive atomic.Int32
	healthy := resolve(snapshot([]any{map[string]any{"id": "wrong-project"}}, []any{thread("healthy", "working"), thread("stopped", "wrong worker"), thread("foreign", "unmanaged")}, nil))
	opts := DispatchOnDemand{
		Controller:  DispatchWorker{Resolve: resolve(snapshot([]any{map[string]any{"id": project}}, nil, nil))},
		ProjectRoot: t.TempDir(), MaxWorkers: 8,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			t.Fatal("shell poll allocated a worker")
			return DispatchWorker{}, "", nil
		},
		Lookup: func(key string) (DispatchWorker, error) {
			w := DispatchWorker{Resolve: func(context.Context) (Upstream, error) {
				active.Add(1)
				return Upstream{}, errors.New("wake requested")
			}}
			switch key {
			case "healthy":
				w.ResolvePassive = healthy
			case "stopped":
				w.ResolvePassive = func(context.Context) (Upstream, error) {
					passive.Add(1)
					return Upstream{}, errors.New("stopped")
				}
			case "unavailable":
				return DispatchWorker{}, errors.New("unavailable provider")
			}
			return w, nil
		},
	}
	const origin = "http://127.0.0.1:38881"
	p, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	d := p.dispatch
	d.disk.OnDemand.Projects[project] = demandProject{ID: project, Title: "Main", Remote: "git@github.com:org/repo.git"}
	for _, id := range []string{"healthy", "stopped", "unavailable", "uncertain", "archived"} {
		d.disk.OnDemand.Routes[id] = demandRoute{Key: id, Ready: true, ProjectID: project}
	}
	for _, id := range []string{"stopped", "unavailable", "archived"} {
		if err := d.saveThreadListing(id, id, &threadListing{Schema: 1, Archived: id == "archived", Thread: thread(id, "last known")}); err != nil {
			t.Fatal(err)
		}
	}
	// Reload the durable native listings. No transport from the old process survives.
	p, err = NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := p.dispatch.onDemandShell(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	projects := shell["projects"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["id"] != project {
		t.Fatalf("worker failure affected project discovery: %v", projects)
	}
	got := map[string]any{}
	for _, raw := range shell["threads"].([]any) {
		row := raw.(map[string]any)
		got[row["id"].(string)] = row["title"]
		if row["id"] != "healthy" && row["latestVisibleMessage"] != nil {
			t.Fatal("stored a message body in the navigation cache")
		}
	}
	if want := map[string]any{"healthy": "working", "stopped": "last known", "unavailable": "last known"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected shell rows: %v", got)
	}
	archived := shell["archivedThreads"].([]any)
	if len(archived) != 1 || archived[0].(map[string]any)["id"] != "archived" {
		t.Fatalf("archived navigation row was lost: %v", archived)
	}
	if active.Load() != 0 || passive.Load() != 1 {
		t.Fatalf("shell used %d waking connections, %d passive checks of stopped worker", active.Load(), passive.Load())
	}
}

func TestExplicitStartRestoresOnlySelectedLegacyThread(t *testing.T) {
	const origin = "http://127.0.0.1:38881"
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	project := demandProject{ID: "repo", Title: "Repo", Remote: "https://github.com/org/repo.git"}
	legacy := dispatchState{Origin: origin, EnvironmentID: "fleet", Clients: map[string]time.Time{}, OnDemand: &onDemandDisk{
		Project: &project,
		Routes: map[string]demandRoute{
			"selected-thread": {Ready: true, ProjectID: project.ID, Key: "selected"},
			"sibling-thread":  {Ready: true, ProjectID: project.ID, Key: "sibling"},
		},
	}}
	if err := store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schemaVersion":1,"threads":[{"id":"selected-thread","projectId":"repo","title":"Recovered"},{"id":"sibling-thread","projectId":"repo","title":"Other"}]}`))
	}))
	defer native.Close()
	opts := DispatchOnDemand{
		Controller: DispatchWorker{Resolve: func(context.Context) (Upstream, error) {
			return Upstream{}, errors.New("no controller access needed for recovery")
		}},
		ProjectRoot: t.TempDir(), MaxWorkers: 2,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			t.Fatal("recovery allocated a worker")
			return DispatchWorker{}, "", nil
		},
		Lookup: func(key string) (DispatchWorker, error) {
			return DispatchWorker{ID: key, Resolve: func(context.Context) (Upstream, error) {
				if key != "selected" {
					t.Fatal("recovery woke a sibling")
				}
				opens.Add(1)
				return Upstream{Endpoint: provider.Endpoint{URL: native.URL}, Token: "test-worker-token"}, nil
			}}, nil
		},
	}
	p, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreWorkerListing(t.Context(), "selected"); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	selected := reopened.dispatch.demandRoute("selected-thread")
	if selected.Listing == nil || selected.Listing.Thread["title"] != "Recovered" || reopened.dispatch.demandRoute("sibling-thread").Listing != nil {
		t.Fatal("restored a missing or unrelated thread listing")
	}
	if err := reopened.RestoreWorkerListing(t.Context(), "selected"); err != nil || opens.Load() != 1 {
		t.Fatal("already recovered listing was fetched again")
	}
}
