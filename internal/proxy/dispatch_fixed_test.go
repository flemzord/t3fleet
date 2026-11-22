package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestFixedSubscriptionWaitsForAnotherRequestToWakeWorker(t *testing.T) {
	requested := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		if err := writeDispatchFrame(r.Context(), conn, map[string]any{
			"_tag": "Chunk", "requestId": "thread", "values": []any{},
		}); err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(server.Close)
	a := NewActivity(time.Now, false)
	worker := DispatchWorker{Activity: a, Resolve: func(context.Context) (Upstream, error) {
		requested <- struct{}{}
		a.Awake()
		return Upstream{Endpoint: provider.Endpoint{URL: server.URL}, Token: "test"}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	chunks := make(chan struct{}, 4)
	ended := make(chan error, 1)
	d := &dispatcher{timeout: time.Second}
	go func() {
		ended <- d.rpcTo(ctx, worker, map[string]any{"_tag": "Request", "id": "thread", "tag": "orchestration.subscribeThread"}, nil,
			func(map[string]any) error { chunks <- struct{}{}; return nil })
	}()
	wait := func(ch <-chan struct{}, label string) {
		t.Helper()
		select {
		case <-ch:
		case err := <-ended:
			t.Fatalf("subscription ended while waiting for %s: %v", label, err)
		case <-ctx.Done():
			t.Fatalf("did not receive %s", label)
		}
	}
	wait(chunks, "initial chunk")
	wait(requested, "initial connection")
	if err := a.Check(0, func() (bool, error) { return false, nil }, func() error { return nil }); err != nil || !a.Sleeping() {
		t.Fatalf("open subscription prevented idle stop: %v", err)
	}
	select {
	case <-requested:
		t.Fatal("subscription woke the stopped worker")
	case err := <-ended:
		t.Fatalf("subscription ended on idle stop: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Model a wake by an explicit request; the subscription can reconnect.
	a.Awake()
	wait(requested, "connection after waking")
	wait(chunks, "new snapshot on the existing subscription")
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("subscription did not exit when cancelled")
	}
}

func TestFixedPlacementUsesLoadAndKeepsStableSlots(t *testing.T) {
	d := &dispatcher{environmentID: "test", disk: dispatchState{OnDemand: &onDemandDisk{Routes: map[string]demandRoute{}}}}
	states := map[string]string{}
	d.onDemand = &DispatchOnDemand{Fixed: &FixedPlacement{Workers: 2, State: func(key string) string {
		if s := states[key]; s != "" {
			return s
		}
		return "absent"
	}}}
	if got := d.chooseFixedSlot(); got != 1 {
		t.Fatalf("first placement: %d", got)
	}
	d.disk.OnDemand.Routes["a"] = demandRoute{PoolSlot: 1}
	if got := d.chooseFixedSlot(); got != 2 {
		t.Fatalf("concurrent launch should avoid the reserved slot, got %d", got)
	}
	d.disk.OnDemand.Routes["a"] = demandRoute{PoolSlot: 1, Ready: true, Listing: &threadListing{Thread: map[string]any{"status": "idle"}}}
	states[d.fixedSlotKey(1)] = "ready"
	if got := d.chooseFixedSlot(); got != 1 {
		t.Fatalf("idle warm slot should win a tie, got %d", got)
	}
	states[d.fixedSlotKey(1)] = "deleted"
	if got := d.chooseFixedSlot(); got != 2 {
		t.Fatalf("deleted slot must not be reused, got %d", got)
	}
}

func TestLiveFixedDispatchSharesWorkerWithoutSharingWorktrees(t *testing.T) {
	setup := setupDispatchReplay(t)
	controllerURL, controllerToken, _ := startLiveT3(t, nil)
	workerURL, workerToken, _ := startLiveT3(t, setup)
	backend := func(id, origin, token string) DispatchWorker {
		return DispatchWorker{ID: id, Resolve: func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: token}, nil
		}}
	}
	worker := backend("fixed", workerURL, workerToken)
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fleet test", "-c", "user.email=fleet@example.invalid", "commit", "-qm", "initial", "--allow-empty"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = workspace
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	origin := "http://" + server.Listener.Addr().String()
	var savedSlot, savedCheckout string
	dedicated := errors.New("fixed thread used dedicated placement")
	opts := DispatchOnDemand{
		Controller: backend("controller", controllerURL, controllerToken), ProjectRoot: t.TempDir(), MaxWorkers: 1,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			return DispatchWorker{}, "", dedicated
		},
		Lookup:  func(string) (DispatchWorker, error) { return DispatchWorker{}, dedicated },
		Release: func(context.Context, string) error { return dedicated },
		Fixed: &FixedPlacement{Workers: 1, State: func(string) string { return "ready" },
			Lookup: func(key string) (DispatchWorker, error) {
				if key != savedSlot {
					return DispatchWorker{}, errors.New("fixed route lost its slot")
				}
				return worker, nil
			},
			Prepare: func(_ context.Context, key, checkout, _ string) (DispatchWorker, string, error) {
				if savedSlot == "" {
					savedSlot, savedCheckout = key, checkout
				}
				if key != savedSlot || checkout != savedCheckout {
					return DispatchWorker{}, "", errors.New("same project failed to reuse its fixed checkout")
				}
				return worker, workspace, nil
			},
		},
	}
	p, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = p
	server.Start()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.dispatch.saveToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	c := newDispatchTestClient(t, ctx, origin, token)
	defer func() { _ = c.conn.CloseNow() }()
	c.rpc("projectClone.start", map[string]any{"projectId": "demo", "title": "Demo", "remoteUrl": "https://github.com/octocat/Hello-World.git"})
	paths := map[string]bool{}
	for _, id := range []string{"fixed-a", "fixed-b"} {
		c.rpc("orchestration.launchThread", map[string]any{"threadId": id, "projectId": "demo", "commandId": "launch-" + id,
			"title": id, "modelSelection": map[string]any{"instanceId": "devReplay", "model": "multi-turn"},
			"runtimeMode": "full-access", "interactionMode": "default",
			"workspaceStrategy": map[string]any{"type": "root"},
			"initialMessage":    map[string]any{"text": "First turn", "messageId": "message-" + id, "attachments": []any{}},
		})
		c.waitReply(id, "first fixture turn complete")
		projection := c.rpc("orchestration.getThreadProjection", map[string]any{"threadId": id})
		thread, _ := projection["thread"].(map[string]any)
		path, _ := thread["worktreePath"].(string)
		if !filepath.IsAbs(path) || path == workspace || paths[path] {
			t.Fatalf("thread %s did not get a distinct worktree: %s", id, path)
		}
		paths[path] = true
	}
	c.rpc("orchestration.dispatchCommand", map[string]any{"type": "thread.archive", "threadId": "fixed-a", "commandId": "archive-a"})
	shell := dispatchHTTP(t, origin, token, dispatchShellPath)
	active, _ := shell["threads"].([]any)
	archived, _ := shell["archivedThreads"].([]any)
	if len(active) != 1 || len(archived) != 1 || archived[0].(map[string]any)["id"] != "fixed-a" {
		t.Fatal("confirmed archive did not move the cached thread out of the active list")
	}
	c.waitReply("fixed-b", "first fixture turn complete")
	// Reopening the catalog with dedicated placement still resolves fixed routes.
	dedicatedMode := *opts.Fixed
	dedicatedMode.Workers = 0
	opts.Fixed = &dedicatedMode
	reopened, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.dispatch.threadWorker("fixed-b")
	if err != nil || got.ID != worker.ID || reopened.dispatch.environmentID != p.dispatch.environmentID {
		t.Fatalf("fixed route changed after switching mode: %v", err)
	}
	var raw dispatchState
	if err := store.Load(&raw); err != nil || raw.OnDemand != nil || raw.Shared == nil {
		t.Fatal("shared state must be rejected by an older dedicated-only daemon")
	}
	if len(savedCheckout) != 32 {
		t.Fatal("missing persisted project checkout key")
	}
}
