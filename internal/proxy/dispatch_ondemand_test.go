package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	if demandRemote("https://github.com/octocat/Hello-World.git") == "" || demandRemote("git@github.com:octocat/Hello-World.git") == "" {
		t.Fatal("normal project remotes rejected")
	}
}

func TestDispatchKeepsHostPolicyWhenDisablingUpdates(t *testing.T) {
	policy := map[string]any{
		"organization": "Platform", "supportUrl": "https://example.org/support", "lockedSettings": []any{"example"},
		"limits": map[string]any{"connect": false, "updates": map[string]any{"mode": "auto", "allowNightly": false}},
	}
	cfg := map[string]any{"managedPolicy": policy}
	dispatchUpdatePolicy(cfg)
	want := map[string]any{
		"organization": "Platform", "supportUrl": "https://example.org/support", "lockedSettings": []any{"example"},
		"limits": map[string]any{"connect": false, "updates": map[string]any{"mode": "off", "allowNightly": false}},
	}
	if !reflect.DeepEqual(cfg["managedPolicy"], want) {
		t.Fatalf("fleet changed the host policy beyond update mode: %#v", cfg["managedPolicy"])
	}
}

func TestDispatchCapabilities(t *testing.T) {
	caps := map[string]any{
		"connectionProbe": true, "commandOutputStreaming": false, "peerLinkPairingCodes": true,
		"mcpModeLimitHeader": true, "linkFence": true,
		"attachmentUploads": true, "fileAttachments": map[string]any{"maxUploadBytes": 10},
		"serverSelfUpdate": map[string]any{}, "peerLinks": true, "futureCapability": true,
		"threadForceStop": true, "pullRequests": true, "agentActivityPublishing": true,
		"shellOlderSettled": true, "shellThreadFamilies": true, "linkedThreadRefreshes": true,
	}
	dispatchCapabilities(caps)
	want := map[string]any{
		"connectionProbe": true, "commandOutputStreaming": false, "peerLinkPairingCodes": true,
		"mcpModeLimitHeader": true, "linkFence": true, "agentActivityPublishing": false,
		"shellOlderSettled": false, "shellThreadFamilies": false, "linkedThreadRefreshes": false,
	}
	if !reflect.DeepEqual(caps, want) {
		t.Fatalf("fleet capabilities = %v, want %v", caps, want)
	}
}

func checkFleetDescriptor(t *testing.T, descriptor map[string]any, version any) {
	t.Helper()
	caps, ok := descriptor["capabilities"].(map[string]any)
	if !ok || version == nil || descriptor["serverVersion"] != version {
		t.Fatal("fleet lost the controller version or capability map")
	}
	for _, key := range []string{"serverSelfUpdate", "serverInstallation", "npmReleaseLookup", "desktopAppUpdate",
		"desktopAppChannelSwitch", "serverSelfUpdateProgress", "serverUpdateStaging", "serverUpdateThreadContinuation",
		"attachmentUploads", "fileAttachments", "pullRequests", "threadForceStop", "peerLinks", "serverBrowser"} {
		if _, advertised := caps[key]; advertised {
			t.Errorf("fleet advertises unsupported capability %s", key)
		}
	}
	for _, key := range []string{"connectionProbe", "commandOutputStreaming", "vcsBranchStatus", "peerLinkPairingCodes", "mcpModeLimitHeader", "linkFence"} {
		if caps[key] != true {
			t.Errorf("fleet lost supported capability %s", key)
		}
	}
}

func waitDispatchProjectionRow(c *dispatchTestClient, threadID, collection, id, status string) map[string]any {
	c.t.Helper()
	return waitDispatchProjectionMatch(c, threadID, collection, func(row map[string]any) bool {
		return row["status"] == status && (id == "" || row["id"] == id)
	})
}

func waitDispatchProjectionMatch(c *dispatchTestClient, threadID, collection string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for {
		projection := c.rpc("orchestration.getThreadProjection", map[string]any{"threadId": threadID})
		rows, _ := projection[collection].([]any)
		for _, value := range rows {
			row, _ := value.(map[string]any)
			if match(row) {
				return row
			}
		}
		select {
		case <-c.ctx.Done():
			c.t.Fatalf("thread %s did not reach the expected state in %s", threadID, collection)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitDispatchCheckpoint(c *dispatchTestClient, threadID, turn string) {
	c.t.Helper()
	waitDispatchProjectionMatch(c, threadID, "checkpoints", func(row map[string]any) bool {
		return row["status"] == "ready" && requestKey(row["appRunOrdinal"]) == turn
	})
}

func TestLiveOnDemandDispatch(t *testing.T) {
	setup := setupDispatchReplay(t)
	controllerURL, controllerToken, controllerDescriptor := startLiveT3(t, nil)
	controller := DispatchWorker{ID: "controller", Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: controllerURL}, Token: controllerToken}, nil
	}}
	type prepared struct {
		worker    DispatchWorker
		workspace string
	}
	var backends []prepared
	for range 3 {
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
	const firstRemote = "https://github.com/octocat/Hello-World.git"
	const secondRemote = "git@github.com:another-org/Hello-World.git"
	var attempts, remotes []string
	var deletions []string
	calls := func() int { mu.Lock(); defer mu.Unlock(); return len(attempts) }
	opts := DispatchOnDemand{Controller: controller, ProjectRoot: t.TempDir(), MaxWorkers: 2, Agents: []DispatchAgent{{Driver: "codex", Models: []string{"gpt-6.1-sol"}}, {Driver: "pi", Models: []string{"deepseek/v4", "glm-5"}}}, Provision: func(_ context.Context, key, remote string) (DispatchWorker, string, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, key)
		remotes = append(remotes, remote)
		if len(attempts) == 1 {
			return DispatchWorker{}, "", errors.New("synthetic lost response")
		}
		p, ok := exp[key]
		if !ok {
			p = backends[len(attempts)-2]
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
	}, Release: func(_ context.Context, key string) error {
		mu.Lock()
		defer mu.Unlock()
		deletions = append(deletions, key)
		if len(deletions) == 1 {
			return errors.New("unconfirmed deletion")
		}
		delete(exp, key)
		return nil
	}}
	dir := t.TempDir()
	restart := func() (*Proxy, func()) {
		disk, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewDispatchOnDemand("http://127.0.0.1:38881", disk, opts)
		if err != nil {
			_ = disk.Close()
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
	t.Run("fleet capabilities and policy across discovery and config snapshots", func(t *testing.T) {
		version := controllerDescriptor["serverVersion"]
		checkFleetDescriptor(t, discovery, version)
		sub := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = sub.conn.CloseNow() }()
		sub.send("cfg", "subscribeServerConfig", nil)
		items, _ := sub.next("cfg")["values"].([]any)
		if len(items) == 0 {
			t.Fatal("missing initial config event")
		}
		event, _ := items[0].(map[string]any)
		snapshot, _ := event["config"].(map[string]any)
		for _, cfg := range []map[string]any{config, snapshot} {
			env, _ := cfg["environment"].(map[string]any)
			checkFleetDescriptor(t, env, version)
			policy, _ := cfg["managedPolicy"].(map[string]any)
			limits, _ := policy["limits"].(map[string]any)
			updates, _ := limits["updates"].(map[string]any)
			if updates["mode"] != "off" || policy["organization"] == "" || policy["organization"] == nil || policy["lockedSettings"] == nil {
				t.Fatal("missing managed update policy in fleet config")
			}
		}
	})
	t.Run("virtual branch reporting does not require a checkout", func(t *testing.T) {
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		conn.send("branch", "subscribeVcsBranch", nil)
		want := []any{map[string]any{"refName": nil, "isRepo": true}}
		if values := conn.next("branch")["values"]; !reflect.DeepEqual(values, want) {
			t.Fatalf("virtual branch snapshot = %v, want %v", values, want)
		}
		conn.rpc("vcs.refreshBranch", nil)
	})
	providers, _ := config["providers"].([]any)
	if len(providers) != 2 {
		t.Fatal("expected one virtual provider per declared agent")
	}
	codex, _ := providers[0].(map[string]any)
	models, _ := codex["models"].([]any)
	if codex["instanceId"] != "codex" || len(models) != 1 || models[0].(map[string]any)["slug"] != "gpt-6.1-sol" {
		t.Fatal("virtual catalog did not advertise the configured Codex model")
	}
	pi, _ := providers[1].(map[string]any)
	models, _ = pi["models"].([]any)
	if pi["instanceId"] != "pi" || pi["driver"] != "pi" || len(models) != 2 || models[1].(map[string]any)["slug"] != "glm-5" ||
		models[0].(map[string]any)["isDefault"] != true || models[1].(map[string]any)["isDefault"] != false {
		t.Fatal("virtual catalog did not advertise the configured Pi models")
	}
	client.rpc("server.probe", nil)
	if calls() != 0 {
		t.Fatal("probe provisioned a Boat worker")
	}
	project := client.rpc("projectClone.start", map[string]any{"projectId": "demo", "title": "Demo", "remoteUrl": "https://github.com/octocat/Hello-World.git", "destinationPath": "~/hello"})
	if project["projectId"] != "demo" {
		t.Fatal("project registration failed")
	}
	another := client.rpc("projectClone.start", map[string]any{"projectId": "other", "title": "Another", "remoteUrl": secondRemote, "destinationPath": "~/hello"})
	if another["projectId"] != "other" || another["cwd"] == project["cwd"] {
		t.Fatalf("projects with the same repository name were not distinct: %#v", another)
	}
	retry := client.rpc("projectClone.start", map[string]any{"projectId": "new-generated-id", "title": "Duplicate", "remoteUrl": "git@github.com:OctoCat/hello-world", "destinationPath": "~/another"})
	if retry["projectId"] != "demo" || retry["cwd"] != project["cwd"] {
		t.Fatalf("a repeated Desktop clone changed project identity: %#v", retry)
	}
	shell := dispatchHTTP(t, server.URL, token, dispatchShellPath)
	projects, _ := shell["projects"].([]any)
	if len(projects) != 2 || calls() != 0 {
		t.Fatal("project registration did not stay local")
	}
	launch := func(c *dispatchTestClient, reqID int, thread string) {
		projectID := "demo"
		model, mode := "multi-turn", "full-access"
		if thread == "on-demand-b" {
			projectID = "other"
		}
		if thread == "on-demand-c" {
			model, mode = "approval", "approval-required"
		}
		c.send(reqID, "orchestration.launchThread", map[string]any{"threadId": thread, "projectId": projectID, "commandId": "launch-" + thread,
			"title": thread, "modelSelection": map[string]any{"instanceId": "devReplay", "model": model}, "runtimeMode": mode, "interactionMode": "default",
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
	if len(remotes) != 3 || remotes[0] != firstRemote || remotes[1] != firstRemote || remotes[2] != secondRemote || len(attempts) != 3 || attempts[0] != attempts[1] || attempts[1] == attempts[2] || len(exp) != 2 {
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
	// These fixture workers expose no passive transport. Real native rows
	// captured during launch still let Desktop reopen each thread after restart.
	recovered := dispatchHTTP(t, server.URL, token, dispatchShellPath)
	recoveredThreads, _ := recovered["threads"].([]any)
	if len(recoveredThreads) != 2 {
		t.Fatalf("restart lost native thread listings: %#v", recoveredThreads)
	}
	waitDispatchCheckpoint(client, "on-demand-b", "1")
	route := p.dispatch.demandRoute("on-demand-b")
	mu.Lock()
	workspace := exp[route.Key].workspace
	mu.Unlock()
	if workspace == "" {
		t.Fatal("missing checkout for second thread")
	}
	if err := os.WriteFile(filepath.Join(workspace, "turn-diff.txt"), []byte("second turn change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.rpc("orchestration.dispatchCommand", map[string]any{
		"type": "message.dispatch", "commandId": "demand-followup", "threadId": "on-demand-b", "messageId": "demand-followup",
		"text": "Continue the second thread", "attachments": []any{}, "createdBy": "user", "creationSource": "web",
		"dispatchMode": map[string]any{"type": "start_immediately"},
	})
	client.waitReply("on-demand-b", "second fixture turn complete")
	t.Run("Desktop can load a turn diff", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		const threadID = "on-demand-b"
		waitDispatchCheckpoint(conn, threadID, "2")
		diff := conn.rpc("orchestration.getTurnDiff", map[string]any{
			"threadId": threadID, "fromTurnCount": 1, "toTurnCount": 2, "ignoreWhitespace": false,
		})
		patch, _ := diff["diff"].(string)
		if diff["threadId"] != threadID || requestKey(diff["fromTurnCount"]) != "1" ||
			requestKey(diff["toTurnCount"]) != "2" || !strings.Contains(patch, "+second turn change") {
			t.Fatalf("unexpected turn diff: %v", diff)
		}
	})
	t.Run("Desktop can stream matches within a thread", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		conn.send("search", "orchestration.searchThreadStream", map[string]any{
			"threadId": "on-demand-b", "query": "second fixture turn complete",
		})
		var partial, complete bool
		for {
			frame := conn.next("search")
			switch frame["_tag"] {
			case "Exit":
				conn.result(frame)
				if !partial || !complete {
					t.Fatal("search did not deliver both the first match and the completed result")
				}
				return
			case "Chunk":
				values, _ := frame["values"].([]any)
				for _, value := range values {
					result, _ := value.(map[string]any)
					match, _ := result["match"].(map[string]any)
					entry, _ := match["entryId"].(string)
					if entry == "" || requestKey(result["totalMatches"]) != "1" {
						t.Fatalf("unexpected search result: %v", result)
					}
					switch result["complete"] {
					case false:
						partial = true
					case true, nil: // The pinned server omits complete on the final result.
						complete = true
					default:
						t.Fatalf("unexpected search completion flag: %v", result)
					}
				}
			default:
				t.Fatalf("unexpected search frame: %v", frame)
			}
		}
	})
	t.Run("Desktop can find matches without streaming", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		for threadID, count := range map[string]string{"on-demand-a": "0", "on-demand-b": "1"} {
			result := conn.rpc("orchestration.searchThread", map[string]any{
				"threadId": threadID, "query": "second fixture turn complete",
			})
			if requestKey(result["totalMatches"]) != count {
				t.Fatalf("search in %s returned an unexpected match count: %v", threadID, result["totalMatches"])
			}
		}
	})
	t.Run("Desktop can change the modes of an existing thread", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		const threadID = "on-demand-b"
		conn.rpc("orchestration.dispatchCommand", map[string]any{
			"type": "thread.runtime-mode.set", "threadId": threadID, "commandId": "desktop-runtime-mode",
			"runtimeMode": "approval-required",
		})
		conn.rpc("orchestration.dispatchCommand", map[string]any{
			"type": "thread.interaction-mode.set", "threadId": threadID, "commandId": "desktop-interaction-mode",
			"interactionMode": "plan",
		})
		projection := conn.rpc("orchestration.getThreadProjection", map[string]any{"threadId": threadID})
		thread, _ := projection["thread"].(map[string]any)
		if thread["runtimeMode"] != "approval-required" || thread["interactionMode"] != "plan" {
			t.Fatalf("worker did not persist the mode changes: runtime=%v, interaction=%v", thread["runtimeMode"], thread["interactionMode"])
		}
	})
	if calls() != 3 {
		t.Fatal("routing an existing thread provisioned again")
	}
	archive := func(req int) map[string]any {
		client.send(req, "orchestration.dispatchCommand", map[string]any{
			"type": "thread.archive", "threadId": "on-demand-a", "commandId": "archive-a",
		})
		return client.next(req)["exit"].(map[string]any)
	}
	if exit := archive(14); exit["_tag"] != "Failure" {
		t.Fatalf("unconfirmed deletion should not acknowledge archive: %#v", exit)
	}
	launch(client, 15, "on-demand-c")
	if exit := client.next(15)["exit"].(map[string]any); exit["_tag"] != "Failure" || calls() != 3 {
		t.Fatalf("a slot was freed before deletion was confirmed: %#v", exit)
	}
	_ = client.conn.CloseNow()
	stop()
	stop = func() {}
	p, stop = restart()
	server.handler.Store(p)
	client = newDispatchTestClient(t, ctx, server.URL, token)
	p.dispatch.retryDispatchArchives(ctx, func(err error) { t.Error(err) })
	if route := p.dispatch.demandRoute("on-demand-a"); route.Archive == nil || !route.Archive.Released {
		t.Fatal("daemon recovery did not finish the deletion pending at restart")
	}
	for _, req := range []int{16, 17} {
		exit := archive(req)
		value, _ := exit["value"].(map[string]any)
		seq, ok := archiveSequence(value)
		if exit["_tag"] != "Success" || !ok || seq == 0 {
			t.Fatalf("archive replay did not preserve the native sequence: %#v", exit)
		}
	}
	mu.Lock()
	correct := len(deletions) == 2 && deletions[0] == attempts[0] && deletions[1] == attempts[0]
	mu.Unlock()
	if !correct {
		t.Fatal("archive retry did not delete exactly the original worker")
	}
	launch(client, 18, "on-demand-a")
	if exit := client.next(18)["exit"].(map[string]any); exit["_tag"] != "Failure" || calls() != 3 {
		t.Fatalf("archived launch recreated its worker: %#v", exit)
	}
	launch(client, 19, "on-demand-c")
	client.result(client.next(19))
	if calls() != 4 {
		t.Fatal("new thread did not get exactly one worker after capacity release")
	}
	t.Run("Desktop can answer a pending approval", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		const threadID = "on-demand-c"
		pending := waitDispatchProjectionRow(conn, threadID, "runtimeRequests", "", "pending")
		id, _ := pending["id"].(string)
		if id == "" {
			t.Fatal("pending approval has no request ID")
		}
		conn.rpc("orchestration.dispatchCommand", map[string]any{
			"type": "runtime-request.respond", "threadId": threadID, "commandId": "desktop-approve", "requestId": id, "decision": "accept",
		})
		waitDispatchProjectionRow(conn, threadID, "runtimeRequests", id, "resolved")
		waitDispatchProjectionRow(conn, threadID, "runs", "", "completed")
	})
	t.Run("Desktop can interrupt a specific run", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		conn := newDispatchTestClient(t, ctx, server.URL, token)
		defer func() { _ = conn.conn.CloseNow() }()
		const threadID, messageID = "on-demand-c", "desktop-interrupt-message"
		conn.rpc("orchestration.dispatchCommand", map[string]any{
			"type": "message.dispatch", "threadId": threadID, "commandId": messageID, "messageId": messageID,
			"text": "Start another approval turn", "attachments": []any{}, "createdBy": "user", "creationSource": "web",
			"dispatchMode": map[string]any{"type": "start_immediately"},
		})
		waitDispatchProjectionRow(conn, threadID, "runtimeRequests", "", "pending")
		projection := conn.rpc("orchestration.getThreadProjection", map[string]any{"threadId": threadID})
		rows, _ := projection["runs"].([]any)
		var targetID, completedID string
		for _, value := range rows {
			row, _ := value.(map[string]any)
			if row["userMessageId"] == messageID {
				targetID, _ = row["id"].(string)
			} else if row["status"] == "completed" {
				completedID, _ = row["id"].(string)
			}
		}
		if targetID == "" || completedID == "" {
			t.Fatal("expected a target run and a previously completed run")
		}
		conn.rpc("orchestration.dispatchCommand", map[string]any{
			"type": "run.interrupt", "threadId": threadID, "commandId": "desktop-interrupt", "runId": targetID, "holdQueue": true,
		})
		waitDispatchProjectionRow(conn, threadID, "runs", targetID, "interrupted")
		waitDispatchProjectionRow(conn, threadID, "runs", completedID, "completed")
	})
	client.rpc("orchestration.getThreadProjection", map[string]any{"threadId": "on-demand-b"})
	worker, err := p.dispatch.threadWorker("on-demand-c")
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := worker.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := newDispatchTestClient(t, ctx, upstream.Endpoint.URL, upstream.Token)
	defer raw.conn.CloseNow()
	for req := 20; req <= 21; req++ {
		raw.send(req, "orchestration.dispatchCommand", map[string]any{
			"type": "thread.archive", "threadId": "on-demand-c", "commandId": fmt.Sprintf("native-%d", req),
		})
		exit := raw.next(req)["exit"].(map[string]any)
		if req == 20 && exit["_tag"] != "Success" || req == 21 && !desktopAlreadyArchived(exit, "on-demand-c", "native-21") {
			t.Fatalf("unexpected native archive result: %#v", exit)
		}
	}
	client.rpc("orchestration.dispatchCommand", map[string]any{
		"type": "thread.archive", "threadId": "on-demand-c", "commandId": "recover-native-archive",
	})
	if route := p.dispatch.demandRoute("on-demand-c"); route.Archive == nil || !route.Archive.Released {
		t.Fatal("previously archived desktop worker was not recovered")
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

func TestOnDemandDispatchHonorsConfiguredCapacity(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	const limit, projectID = 100, "project"
	reachedProvisioner := errors.New("provisioner reached")
	p, err := NewDispatchOnDemand("http://127.0.0.1:38881", store, DispatchOnDemand{
		Controller:  DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }},
		ProjectRoot: t.TempDir(),
		MaxWorkers:  limit,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			return DispatchWorker{}, "", reachedProvisioner
		},
		Lookup: func(string) (DispatchWorker, error) { t.Fatal("unexpected lookup"); return DispatchWorker{}, nil },
	})
	if err != nil {
		t.Fatalf("dispatcher rejected a capacity of %d: %v", limit, err)
	}
	d := p.dispatch
	d.disk.OnDemand.Projects[projectID] = demandProject{ID: projectID, Title: "Project"}
	for i := range limit - 1 {
		id := fmt.Sprintf("reserved-%d", i)
		d.disk.OnDemand.Routes[id] = demandRoute{Key: id, ProjectID: projectID, Ready: true}
	}

	if _, err := d.prepareDemandWorker(t.Context(), "last-slot", projectID); !errors.Is(err, reachedProvisioner) {
		t.Fatalf("the final configured slot should admit a launch: %v", err)
	}
	if _, err := d.prepareDemandWorker(t.Context(), "over-capacity", projectID); err == nil || errors.Is(err, reachedProvisioner) {
		t.Fatalf("a launch above the configured capacity should be rejected: %v", err)
	}
}
