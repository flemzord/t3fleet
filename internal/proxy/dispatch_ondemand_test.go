package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestDispatchRemoteDoesNotAcceptCredentials(t *testing.T) {
	for _, raw := range []string{"https://user:token@github.com/org/repo", "git@github.com:org/..", "https://github.com/org/repo?token=secret", "file:///tmp/repo", "https://github.com.evil.example/org/repo", "--upload-pack=/bin/bash", "https://github.com/org/%2e%2e"} {
		if demandRemote(raw) != "" {
			t.Fatalf("accepted unsafe remote %q", raw)
		}
	}
	if demandRemote("https://github.com/formancehq/ledger.git") == "" || demandRemote("git@github.com:formancehq/ledger.git") == "" {
		t.Fatal("normal project remotes rejected")
	}
}

func TestLiveOnDemandDispatch(t *testing.T) {
	setup := setupDispatchReplay(t)
	controllerURL, controllerToken, _ := startLiveT3(t, nil)
	controller := DispatchWorker{ID: "controller", Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: controllerURL}, Token: controllerToken}, nil
	}}
	type prepared struct {
		worker    DispatchWorker
		workspace string
	}
	var backends []prepared
	for range 2 {
		origin, token, _ := startLiveT3(t, setup)
		up := Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: token}
		workspace := t.TempDir()
		if err := exec.CommandContext(t.Context(), "git", "init", "-q", workspace).Run(); err != nil {
			t.Fatal(err)
		}
		backends = append(backends, prepared{DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return up, nil }}, workspace})
	}
	var mu sync.Mutex
	exp := map[string]prepared{}
	var attempts []string
	calls := func() int { mu.Lock(); defer mu.Unlock(); return len(attempts) }
	opts := DispatchOnDemand{Controller: controller, ProjectRoot: t.TempDir(), MaxWorkers: 2, Model: "gpt-6.1-sol", Provision: func(_ context.Context, key, remote string) (DispatchWorker, string, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, key)
		if remote != "https://github.com/octocat/Hello-World.git" {
			return DispatchWorker{}, "", errors.New("project remote did not reach provisioner")
		}
		if len(attempts) == 1 {
			return DispatchWorker{}, "", errors.New("synthetic lost response")
		}
		p, ok := exp[key]
		if !ok {
			p = backends[len(exp)]
			exp[key] = p
		}
		return p.worker, p.workspace, nil
	}, Lookup: func(key string) (DispatchWorker, error) {
		mu.Lock()
		defer mu.Unlock()
		p, ok := exp[key]
		if !ok {
			return DispatchWorker{}, errors.New("worker absent")
		}
		return p.worker, nil
	}}
	dir := t.TempDir()
	restart := func() (*Proxy, func()) {
		disk, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewDispatchOnDemand("http://127.0.0.1:38881", disk, opts)
		if err != nil {
			disk.Close()
			t.Fatal(err)
		}
		return p, func() { _ = disk.Close() }
	}
	p, stop := restart()
	defer func() { stop() }()
	// Use an actual websocket from the same test server helper; virtual origin
	// remains fixed through both restarts.
	server := newDispatchRestartTestServer(t, p)
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.dispatch.saveToken(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	client := newDispatchTestClient(t, ctx, server.URL, token)
	defer func() { _ = client.conn.CloseNow() }()
	discovery := dispatchHTTP(t, server.URL, "", "/.well-known/t3/environment")
	config := client.rpc("server.getConfig", nil)
	environment, _ := config["environment"].(map[string]any)
	if discovery["label"] != "t3fleet" || environment["label"] != "t3fleet" ||
		environment["environmentId"] != discovery["environmentId"] {
		t.Fatal("virtual environment name or identity differs between discovery and configuration")
	}
	providers, _ := config["providers"].([]any)
	if len(providers) != 1 {
		t.Fatal("expected a single virtual Codex provider")
	}
	codex, _ := providers[0].(map[string]any)
	models, _ := codex["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["slug"] != "gpt-6.1-sol" {
		t.Fatal("virtual catalog did not advertise the configured Codex model")
	}
	client.rpc("server.probe", nil)
	if calls() != 0 {
		t.Fatal("probe provisioned a Boat worker")
	}
	project := client.rpc("projectClone.start", map[string]any{"projectId": "demo", "title": "Demo", "remoteUrl": "https://github.com/octocat/Hello-World.git", "destinationPath": "~/hello"})
	if project["projectId"] != "demo" {
		t.Fatal("project registration failed")
	}
	shell := dispatchHTTP(t, server.URL, token, dispatchShellPath)
	projects, _ := shell["projects"].([]any)
	if len(projects) != 1 || calls() != 0 {
		t.Fatal("project registration did not stay local")
	}
	launch := func(c *dispatchTestClient, reqID int, thread string) {
		c.send(reqID, "orchestration.launchThread", map[string]any{"threadId": thread, "projectId": "demo", "commandId": "launch-" + thread,
			"title": thread, "modelSelection": map[string]any{"instanceId": "devReplay", "model": "multi-turn"}, "runtimeMode": "full-access", "interactionMode": "default",
			"workspaceStrategy": map[string]any{"type": "root"},
			"initialMessage":    map[string]any{"text": "One", "messageId": "message-" + thread, "attachments": []any{}},
		})
	}
	launch(client, 11, "on-demand-a")
	failure := client.next(11)["exit"].(map[string]any)
	if failure["_tag"] != "Failure" || calls() != 1 {
		t.Fatal("interrupted preparation did not fail after one reservation")
	}
	_ = client.conn.CloseNow()
	stop()
	stop = func() {}
	p, stop = restart()
	server.handler.Store(p)
	after := dispatchHTTP(t, server.URL, "", "/.well-known/t3/environment")
	if after["label"] != "t3fleet" || after["environmentId"] != discovery["environmentId"] {
		t.Fatal("virtual environment identity changed on restart")
	}
	client = newDispatchTestClient(t, ctx, server.URL, token)
	launch(client, 12, "on-demand-a")
	client.result(client.next(12))
	launch(client, 13, "on-demand-b")
	client.result(client.next(13))
	mu.Lock()
	if len(attempts) != 3 || attempts[0] != attempts[1] || attempts[1] == attempts[2] || len(exp) != 2 {
		mu.Unlock()
		t.Fatal("retry allocated another worker or different threads shared one reservation")
	}
	mu.Unlock()
	client.rpc("orchestration.getThreadProjection", map[string]any{"threadId": "on-demand-a"})
	client.rpc("orchestration.getThreadProjection", map[string]any{"threadId": "on-demand-b"})
	client.waitReply("on-demand-a", "first fixture turn complete")
	client.waitReply("on-demand-b", "first fixture turn complete")
	_ = client.conn.CloseNow()
	stop()
	stop = func() {}
	p, stop = restart()
	server.handler.Store(p)
	client = newDispatchTestClient(t, ctx, server.URL, token)
	client.rpc("orchestration.dispatchCommand", map[string]any{
		"type": "message.dispatch", "commandId": "demand-followup", "threadId": "on-demand-b", "messageId": "demand-followup",
		"text": "Continue the second thread", "attachments": []any{}, "createdBy": "user", "creationSource": "web",
		"dispatchMode": map[string]any{"type": "start_immediately"},
	})
	client.waitReply("on-demand-b", "second fixture turn complete")
	if calls() != 3 {
		t.Fatal("routing an existing thread provisioned again")
	}
}

// This small wrapper changes the handler without changing the client origin.
type dispatchRestartTestServer struct {
	URL     string
	handler atomic.Pointer[Proxy]
}

func newDispatchRestartTestServer(t *testing.T, p *Proxy) *dispatchRestartTestServer {
	t.Helper()
	rs := new(dispatchRestartTestServer)
	rs.handler.Store(p)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The virtual public origin is stable on disk; adapt the httptest Host.
		r.Host = "127.0.0.1:38881"
		rs.handler.Load().ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	rs.URL = s.URL
	return rs
}
