package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

// Exercise a native source's peer client, the proxy, and two native worker
// servers with the replay fixture. No Boat or commercial model login is used.
func TestLiveLinkedDispatch(t *testing.T) {
	replay := setupDispatchReplay(t)
	workerSetup := func(base string) {
		replay(base)
		path := filepath.Join(base, "userdata", "settings.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(settings["providerInstances"], &providers); err != nil {
			t.Fatal(err)
		}
		// The virtual fleet advertises one Codex slot. In this fixture only,
		// attach the replay agent to that slot to exercise the same routing.
		settings["providerInstances"], err = json.Marshal(map[string]json.RawMessage{"codex": providers["devReplay"]})
		if err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	enableLinks := func(base string) {
		path := filepath.Join(base, "userdata")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "settings.json"), []byte(`{"linkedEnvironmentsEnabled":true}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	genuine := func(id string, setup func(string)) (DispatchWorker, string, string) {
		address, bearer, _ := startLiveT3(t, setup)
		return DispatchWorker{ID: id, Resolve: func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: address}, Token: bearer}, nil
		}}, address, bearer
	}
	source, sourceOrigin, sourceBearer := genuine("source", enableLinks)
	controller, _, _ := genuine("controller", enableLinks)
	type prepared struct {
		worker    DispatchWorker
		workspace string
	}
	var workers []prepared
	for i := range 2 {
		worker, _, _ := genuine(fmt.Sprintf("worker-%d", i), workerSetup)
		workspace := t.TempDir()
		if err := exec.CommandContext(t.Context(), "git", "init", "-q", workspace).Run(); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, prepared{worker, workspace})
	}
	var mu sync.Mutex
	routes := map[string]prepared{}
	attempts := 0
	var releases []string
	opts := DispatchOnDemand{
		Controller: controller, ProjectRoot: t.TempDir(), MaxWorkers: len(workers), Model: "multi-turn",
		Provision: func(_ context.Context, key, _ string) (DispatchWorker, string, error) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			worker, ok := routes[key]
			if !ok {
				if len(routes) >= len(workers) {
					return DispatchWorker{}, "", fmt.Errorf("unexpected additional allocation")
				}
				worker = workers[len(routes)]
				routes[key] = worker
			}
			return worker.worker, worker.workspace, nil
		},
		Lookup: func(key string) (DispatchWorker, error) {
			mu.Lock()
			defer mu.Unlock()
			if worker, ok := routes[key]; ok {
				return worker.worker, nil
			}
			return DispatchWorker{}, fmt.Errorf("unknown fixture reservation")
		},
		Release: func(_ context.Context, key string) error {
			mu.Lock()
			defer mu.Unlock()
			releases = append(releases, key)
			delete(routes, key)
			return nil
		},
	}
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	origin := "http://" + server.Listener.Addr().String()
	storeDir := t.TempDir()
	store, err := state.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	gateway, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	var proxyMu sync.RWMutex
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.RLock()
		current := gateway
		proxyMu.RUnlock()
		current.ServeHTTP(w, r)
	})
	server.Start()

	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	desktop, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.dispatch.saveToken(ctx, desktop); err != nil {
		t.Fatal(err)
	}
	web := newDispatchTestClient(t, ctx, origin, desktop)
	defer web.conn.CloseNow()
	project := web.rpc("projectClone.start", map[string]any{
		"projectId": "demo", "title": "Demo", "remoteUrl": "https://github.com/octocat/Hello-World.git", "destinationPath": "~/hello",
	})
	if project["projectId"] != "demo" {
		t.Fatal("virtual project was not ready")
	}
	_ = web.conn.CloseNow()
	descriptor := dispatchHTTP(t, origin, "", "/.well-known/t3/environment")
	public := DispatchWorker{ID: "gateway", Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: desktop}, nil
	}}
	publicUp, err := public.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pairing struct{ Credential string }
	if err := workerPeerJSON(ctx, http.DefaultClient, publicUp, desktop, http.MethodPost, "/api/auth/pairing-token",
		map[string]string{"purpose": "peer-link", "access": "approval-required"}, &pairing); err != nil || pairing.Credential == "" {
		t.Fatalf("could not mint gateway pairing code: %v", err)
	}
	sourceWS := newDispatchTestClient(t, ctx, sourceOrigin, sourceBearer)
	defer sourceWS.conn.CloseNow()
	link := sourceWS.rpc("peerLinks.link", map[string]any{
		"url": origin, "pairingCode": pairing.Credential, "access": "approval-required",
	})
	if link["environmentId"] != descriptor["environmentId"] || link["access"] != "approval-required" {
		t.Fatal("the source linked an unexpected environment or access level")
	}
	sourceToken, err := gateway.dispatch.mintWorkerPeer(ctx, source, "full-access", "test source CLI")
	if err != nil {
		t.Fatal(err)
	}
	tool := func(worker DispatchWorker, bearer, name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		headers := http.Header{"T3-Mode-Limit": {"full-access/default"}}
		raw, err := gateway.dispatch.callWorkerPeer(ctx, worker, bearer, headers, request)
		if err != nil {
			t.Fatalf("%s failed at the MCP transport: %v", name, err)
		}
		var result struct {
			Result struct {
				Content []struct{ Text string }
				Data    map[string]any `json:"structuredContent"`
				IsError bool
			}
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("%s returned malformed output", name)
		}
		if result.Result.IsError && len(result.Result.Content) > 0 {
			return nil, result.Result.Content[0].Text
		}
		if result.Result.Data == nil {
			t.Fatalf("%s returned no structured result: %s", name, raw)
		}
		return result.Result.Data, ""
	}
	remote := func(name string, args map[string]any) map[string]any {
		t.Helper()
		args["environmentId"] = link["environmentId"]
		got, failure := tool(source, sourceToken, name, args)
		if failure != "" {
			t.Fatalf("source's %s failed: %s", name, failure)
		}
		return got
	}
	launch := func(key string) string {
		t.Helper()
		got := remote("t3_thread_launch", map[string]any{
			"projectId": "demo", "title": key, "message": "First replay turn", "clientRequestId": key,
			"runtimeMode": "approval-required", "workspaceStrategy": map[string]any{"type": "root"},
		})
		id, _ := got["threadId"].(string)
		if id == "" || got["link"] == nil {
			t.Fatalf("launch of %s did not return a thread and link: %#v", key, got)
		}
		return id
	}
	first, second := launch("first-linked-canary"), launch("second-linked-canary")
	if first == second {
		t.Fatal("different remote launches returned the same native thread")
	}
	for _, id := range []string{first, second} {
		wait := remote("t3_thread_wait", map[string]any{"threadId": id, "timeoutMs": 15000})
		if wait["status"] != "completed" || wait["timedOut"] != false {
			t.Fatalf("native worker did not complete: %#v", wait)
		}
		detail := remote("t3_thread_read", map[string]any{"threadId": id})
		raw, _ := json.Marshal(detail)
		if !bytes.Contains(raw, []byte("first fixture turn complete")) {
			t.Fatalf("the source could not read the replay response for %s", id)
		}
	}
	var found []string
	for _, worker := range workers {
		shell, err := gateway.dispatch.getFrom(ctx, worker.worker, dispatchShellPath)
		if err != nil {
			t.Fatal(err)
		}
		threads, _ := shell["threads"].([]any)
		if len(threads) != 1 {
			t.Fatalf("%s has %d threads, expected one", worker.worker.ID, len(threads))
		}
		thread := threads[0].(map[string]any)
		found = append(found, thread["id"].(string))
		if fence, _ := thread["linkOrigin"].(map[string]any); fence["access"] != "approval-required" || fence["sessionId"] == "" {
			t.Fatalf("%s did not persist the native peer fence", worker.worker.ID)
		}
	}
	if !slices.Contains(found, first) || !slices.Contains(found, second) {
		t.Fatal("native workers did not host the exact threads returned to the source")
	}
	// Restart the gateway, preserve its origin and the source's saved link.
	proxyMu.Lock()
	err = store.Close()
	if err == nil {
		store, err = state.Open(storeDir)
	}
	if err == nil {
		gateway, err = NewDispatchOnDemand(origin, store, opts)
	}
	proxyMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if replayed := launch("first-linked-canary"); replayed != first {
		t.Fatal("retry after restart forked a native thread")
	}
	remote("t3_thread_send", map[string]any{"threadId": second, "message": "Follow up after restart"})
	if wait := remote("t3_thread_wait", map[string]any{"threadId": second, "timeoutMs": 15000}); wait["status"] != "completed" {
		t.Fatalf("follow-up did not complete: %#v", wait)
	}
	if detail, _ := json.Marshal(remote("t3_thread_read", map[string]any{"threadId": second})); !bytes.Contains(detail, []byte("second fixture turn complete")) {
		t.Fatal("the source could not read the follow-up reply")
	}

	// A different linked session cannot mutate the first session's threads.
	other, err := gateway.dispatch.mintNativeWorkerPeer(ctx, public, "approval-required", "unrelated canary peer")
	if err != nil {
		t.Fatal(err)
	}
	if _, failure := tool(public, other, "t3_thread_send", map[string]any{"threadId": first, "message": "Must not be sent"}); failure == "" {
		t.Fatal("another linked session mutated the first session's thread")
	}
	if _, failure := tool(public, other, "t3_environment_read", map[string]any{}); failure != "" {
		t.Fatalf("second peer was not reachable before revocation: %s", failure)
	}
	grant, ok := parsePeerNativeSession(other)
	if !ok {
		t.Fatal("second peer did not receive a native grant")
	}
	var revoke struct{ Revoked bool }
	if err := workerPeerJSON(ctx, http.DefaultClient, publicUp, desktop, http.MethodPost, "/api/auth/clients/revoke",
		map[string]string{"sessionId": grant.SessionID}, &revoke); err != nil || !revoke.Revoked {
		t.Fatalf("could not revoke the second peer: %v", err)
	}
	if _, err := gateway.dispatch.callWorkerPeer(ctx, public, other, nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err == nil {
		t.Fatal("gateway accepted a revoked controller bearer at /mcp")
	}
	// Desktop and linked MCP credentials are not interchangeable.
	if _, err := gateway.dispatch.callWorkerPeer(ctx, public, desktop, nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err == nil {
		t.Fatal("gateway accepted a Desktop credential at /mcp")
	}
	// First archive without going through cleanup, simulating a lost result
	// between the native archive and the dispatcher's durable acknowledgement.
	route := gateway.dispatch.demandRoute(first)
	worker, err := gateway.dispatch.threadWorker(first)
	if err != nil {
		t.Fatal(err)
	}
	if result, failure := tool(worker, route.Peer.WorkerToken, "t3_thread_organize",
		map[string]any{"threadId": first, "action": "archive"}); failure != "" || result["sequence"] == nil {
		t.Fatalf("native archive failed: %s %#v", failure, result)
	}
	for range 2 {
		result := remote("t3_thread_organize", map[string]any{"threadId": first, "action": "archive"})
		if result["sequence"] == nil {
			t.Fatalf("archive did not confirm release: %#v", result)
		}
	}
	remote("t3_thread_read", map[string]any{"threadId": second})
	mu.Lock()
	defer mu.Unlock()
	if len(releases) != 1 || releases[0] != route.Key {
		t.Fatalf("wrong or repeated worker release: %v", releases)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly two provisioner calls, got %d", attempts)
	}
}
