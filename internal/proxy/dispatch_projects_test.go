package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
)

func TestDemandProjectMigrationAndRetryFences(t *testing.T) {
	t.Parallel()
	const origin = "http://127.0.0.1:38881"
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	first := demandProject{ID: "api", Title: "API", Remote: "git@github.com:Org/API.git"}
	second := demandProject{ID: "web", Title: "Web", Remote: "https://github.com/Other/API.git"}

	// Real single-project state, including a client and an interrupted launch.
	stateBefore := dispatchState{Origin: origin, EnvironmentID: "old-env", Clients: map[string]time.Time{"existing": time.Now().Add(time.Hour)},
		OnDemand: &onDemandDisk{Project: &first, Routes: map[string]demandRoute{"thread": {Key: "saved-key", ProjectID: first.ID}}}}
	if err := store.Save(stateBefore); err != nil {
		t.Fatal(err)
	}
	type attempt struct{ key, remote string }
	var calls []attempt
	reachedProvider := errors.New("provisioner reached")
	opts := DispatchOnDemand{
		ProjectRoot: t.TempDir(), MaxWorkers: 10,
		Controller: DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, errors.New("no controller") }},
		Provision: func(_ context.Context, key, remote string) (DispatchWorker, string, error) {
			calls = append(calls, attempt{key, remote})
			return DispatchWorker{}, "", reachedProvider
		},
		Lookup: func(string) (DispatchWorker, error) {
			t.Fatal("unready assignment was used")
			return DispatchWorker{}, nil
		},
	}
	open := func() *dispatcher {
		p, err := NewDispatchOnDemand(origin, store, opts)
		if err != nil {
			t.Fatal(err)
		}
		return p.dispatch
	}
	d := open()
	if d.disk.EnvironmentID != "old-env" || len(d.disk.Clients) != 1 || d.disk.OnDemand.Project != nil {
		t.Fatal("failed to migrate the existing environment and its pairing")
	}
	if got, err := d.rememberProject(second); err != nil || got != second {
		t.Fatalf("register another project: %#v, %v", got, err)
	}
	same := demandProject{ID: "fresh-id", Title: "Renamed?", Remote: "https://github.com/org/api"}
	if got, err := d.rememberProject(same); err != nil || got != first {
		t.Fatalf("expected original identity and transport for same repository, got %#v, %v", got, err)
	}
	same.ID = second.ID
	if _, err := d.rememberProject(same); err == nil {
		t.Fatal("an existing project id was rebound")
	}
	d = open()
	if _, err := d.prepareDemandWorker(t.Context(), "thread", second.ID); err == nil || len(calls) != 0 {
		t.Fatal("retry of an interrupted launch switched to the other repo")
	}
	if _, err := d.prepareDemandWorker(t.Context(), "thread", first.ID); !errors.Is(err, reachedProvider) ||
		len(calls) != 1 || calls[0] != (attempt{"saved-key", first.Remote}) {
		t.Fatalf("original launch did not reuse its saved worker and remote: %v %v", calls, err)
	}
	if _, err := d.prepareDemandWorker(t.Context(), "new", second.ID); !errors.Is(err, reachedProvider) ||
		len(calls) != 2 || calls[1].key == calls[0].key || calls[1].remote != second.Remote {
		t.Fatalf("new launch did not use the second repository: %v %v", calls, err)
	}
}

func TestPeerProjectMustUseReturnedClonePath(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	d := &dispatcher{store: store, disk: dispatchState{OnDemand: &onDemandDisk{Projects: map[string]demandProject{}}},
		onDemand: &DispatchOnDemand{ProjectRoot: t.TempDir(),
			Controller: DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, errors.New("offline") }}}}
	clone := func(repo, destination string) string {
		t.Helper()
		result, err := d.peerProject(t.Context(), peerTool{Name: "t3_project_clone", Arguments: map[string]any{
			"repository": repo, "destinationPath": destination,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return result["cwd"].(string)
	}
	const destination = "/arbitrary/controller/path"
	path := clone("org/repo", destination)
	if path == destination || clone("Org/Repo", "~/elsewhere") != path {
		t.Fatal("clone adopted the requested location or did not reuse the repository")
	}
	if got, err := d.peerProject(t.Context(), peerTool{Name: "t3_project_create", Arguments: map[string]any{
		"title": "Repo", "workspaceRoot": destination,
	}}); err == nil || got != nil {
		t.Fatal("accepted an unrelated server directory")
	}
	if _, err := d.peerProject(t.Context(), peerTool{Name: "t3_project_create", Arguments: map[string]any{
		"title": "Repo", "workspaceRoot": path,
	}}); err == nil {
		t.Fatal("registration with an offline controller reported success")
	}
	for id := range d.disk.OnDemand.Projects {
		if _, err := d.prepareDemandWorker(t.Context(), "thread", id); err == nil {
			t.Fatal("launched a worker for a clone before registration")
		}
	}
}
