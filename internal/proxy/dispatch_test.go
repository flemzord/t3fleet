package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestDispatchAssignmentPersistence(t *testing.T) {
	path := t.TempDir()
	worker := func(context.Context) (Upstream, error) { return Upstream{}, errors.New("offline") }
	workers := []DispatchWorker{{ID: "boat", Resolve: worker}, {ID: "namespace", Resolve: worker}}
	disk, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewDispatchPOC("http://127.0.0.1:44321", disk, workers)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a", "b", "a"} {
		got, err := first.dispatch.worker(id, true)
		if err != nil || got != i%2 {
			t.Fatalf("assignment %s: worker=%d err=%v", id, got, err)
		}
	}
	if _, err := first.dispatch.worker("c", true); err == nil {
		t.Fatal("assigned more threads than the pool can hold")
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disk.Close() }()
	second, err := NewDispatchPOC("http://127.0.0.1:44321", disk, workers)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.dispatch.worker("b", false); err != nil || got != 1 {
		t.Fatal("second assignment was lost")
	}
	if _, err := NewDispatchPOC("http://127.0.0.1:44321", disk, []DispatchWorker{workers[1], workers[0]}); err == nil {
		t.Fatal("a reordered pool changed the durable assignment")
	}
	if _, err := second.dispatch.worker("unknown", false); err == nil {
		t.Fatal("an unknown thread was silently routed")
	}
}

func setupDispatchReplay(t *testing.T) func(string) {
	t.Helper()
	source := os.Getenv("T3FLEET_T3_SOURCE")
	if source == "" {
		t.Skip("set T3FLEET_T3_BINARY and T3FLEET_T3_SOURCE for the genuine dispatch canary")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("the upstream replay agent requires Bun")
	}
	replay := filepath.Join(source, "apps", "server", "scripts", "dev-replay-agent.ts")
	if _, err := os.Stat(replay); err != nil {
		t.Fatal("upstream replay agent not found")
	}
	settings, err := json.Marshal(map[string]any{"providerInstances": map[string]any{"devReplay": map[string]any{
		"driver": "acpRegistry", "displayName": "Replay",
		"config": map[string]any{"source": "local", "commandPath": bun, "commandArgs": []string{replay}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return func(base string) {
		dir := filepath.Join(base, "userdata")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), settings, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// This test uses two real T3 processes and replays ACP turns using the pinned
// upstream's deterministic agent. No cloud or model credentials are needed.
func TestLiveDispatchPOC(t *testing.T) {
	setup := setupDispatchReplay(t)
	var workers []DispatchWorker
	var direct []Upstream
	for _, id := range []string{"worker-a", "worker-b"} {
		origin, token, _ := startLiveT3(t, setup)
		up := Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: token}
		direct = append(direct, up)
		workers = append(workers, DispatchWorker{ID: id, Resolve: func(context.Context) (Upstream, error) { return up, nil }})
	}
	workspace := t.TempDir()
	git := exec.CommandContext(t.Context(), "git", "init", "-q", workspace)
	if err := git.Run(); err != nil {
		t.Fatal("could not initialize disposable workspace")
	}
	dir := t.TempDir()
	startGateway := func(address string) (string, *Proxy, func()) {
		t.Helper()
		ln, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := state.Open(dir)
		if err != nil {
			_ = ln.Close()
			t.Fatal(err)
		}
		origin := "http://" + ln.Addr().String()
		p, err := NewDispatchPOC(origin, disk, workers)
		if err != nil {
			_ = disk.Close()
			_ = ln.Close()
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		srv := &http.Server{Handler: p, BaseContext: func(net.Listener) context.Context { return ctx }}
		done := make(chan struct{})
		go func() { defer close(done); _ = srv.Serve(ln) }()
		return origin, p, func() { cancel(); _ = srv.Close(); <-done; _ = disk.Close() }
	}
	origin, gateway, closeGateway := startGateway("127.0.0.1:0")
	defer func() { closeGateway() }()
	link, err := gateway.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	pairURL, _ := url.Parse(link.URL)
	fragment, _ := url.ParseQuery(pairURL.Fragment)
	res, err := http.PostForm(origin+"/oauth/token", url.Values{"subject_token": {fragment.Get("token")}})
	if err != nil {
		t.Fatal("pair exchange failed")
	}
	creds := decodeDispatchResponse(t, res)
	token, _ := creds["access_token"].(string)
	if token == "" {
		t.Fatal("pair exchange did not issue a token")
	}
	descriptor := dispatchHTTP(t, origin, "", "/.well-known/t3/environment")
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Second)
	defer cancel()
	client := newDispatchTestClient(t, ctx, origin, token)
	defer func() { _ = client.conn.CloseNow() }()
	cfg := client.rpc("server.getConfig", nil)
	env, _ := cfg["environment"].(map[string]any)
	if env["environmentId"] != descriptor["environmentId"] || cfg["directEndpoints"] != nil {
		t.Fatal("virtual identity differs across discovery and RPC")
	}
	// Desktop establishes its configuration subscription before probing the
	// connection. Both must work through the same virtual WebSocket.
	client.send(0, "subscribeServerConfig", map[string]any{"workspaceSnapshotsOnDemand": true})
	configFrame := client.next(0)
	values, _ := configFrame["values"].([]any)
	if len(values) == 0 {
		t.Fatal("desktop configuration subscription did not return a snapshot")
	}
	event, _ := values[0].(map[string]any)
	config, _ := event["config"].(map[string]any)
	environment, _ := config["environment"].(map[string]any)
	if event["type"] != "snapshot" || environment["environmentId"] != descriptor["environmentId"] {
		t.Fatal("desktop configuration subscription returned the wrong environment")
	}
	client.send(1, "server.probe", nil)
	client.result(client.next(1))
	client.rpc("projects.mutate", map[string]any{"type": "project.create", "commandId": "p-create", "projectId": "dispatch-project", "title": "Dispatch", "workspaceRoot": workspace})
	client.send(2, "orchestration.subscribeShell", map[string]any{"afterSequence": 99999, "httpSnapshotFallback": true, "requestCompletionMarker": true})
	initial := client.next(2)
	if !bytes.Contains(marshalDispatch(t, initial), []byte("synchronized")) {
		t.Fatal("virtual shell did not synchronize")
	}
	model := map[string]any{"instanceId": "devReplay", "model": "multi-turn"}
	for _, id := range []string{"dispatch-a", "dispatch-b"} {
		client.send("launch-"+id, "orchestration.launchThread", map[string]any{
			"commandId": "launch-" + id, "threadId": id, "projectId": "dispatch-project", "title": id,
			"modelSelection": model, "runtimeMode": "full-access", "interactionMode": "default",
			"workspaceStrategy": map[string]any{"type": "root"},
			"initialMessage":    map[string]any{"messageId": "first-" + id, "text": "First from " + id, "attachments": []any{}},
		})
	}
	for _, id := range []string{"dispatch-a", "dispatch-b"} {
		value := client.result(client.next("launch-" + id))
		if value["threadId"] != id {
			t.Fatal("launch response crossed threads")
		}
		client.waitReply(id, "first fixture turn complete")
	}
	// Existing active shell subscription must receive the merged view.
	for {
		raw := marshalDispatch(t, client.next(2))
		if bytes.Contains(raw, []byte("dispatch-a")) && bytes.Contains(raw, []byte("dispatch-b")) {
			break
		}
	}
	seen := map[string]bool{}
	for _, up := range direct {
		shell := dispatchHTTP(t, up.Endpoint.URL, up.Token, dispatchShellPath)
		threads, _ := shell["threads"].([]any)
		if len(threads) != 1 {
			t.Fatalf("expected one thread on each genuine server, got %d", len(threads))
		}
		thread, _ := threads[0].(map[string]any)
		id, _ := thread["id"].(string)
		if id != "dispatch-a" && id != "dispatch-b" || seen[id] {
			t.Fatal("threads did not land on different servers")
		}
		seen[id] = true
	}
	// Shut the listener and private store down, then reopen both on the same
	// address with the same bearer. Do not ask for another pairing.
	_ = client.conn.CloseNow()
	closeGateway()
	closeGateway = func() {}
	_, _, closeGateway = startGateway(strings.TrimPrefix(origin, "http://"))
	descriptorAfter := dispatchHTTP(t, origin, "", "/.well-known/t3/environment")
	if descriptorAfter["environmentId"] != descriptor["environmentId"] {
		t.Fatal("virtual identity changed after restart")
	}
	client = newDispatchTestClient(t, ctx, origin, token)
	client.send("detail-b", "orchestration.subscribeThread", map[string]any{
		"threadId": "dispatch-b", "requestCompletionMarker": true,
	})
	initialDetail := marshalDispatch(t, client.next("detail-b"))
	if !bytes.Contains(initialDetail, []byte("first-dispatch-b")) || bytes.Contains(initialDetail, []byte("first-dispatch-a")) {
		t.Fatal("thread subscription did not return the assigned backend's history")
	}
	client.rpc("orchestration.dispatchCommand", map[string]any{
		"type": "message.dispatch", "commandId": "follow-up", "threadId": "dispatch-b", "messageId": "follow-up-m",
		"text": "Continue the second thread", "attachments": []any{}, "createdBy": "user", "creationSource": "web",
		"dispatchMode": map[string]any{"type": "start_immediately"},
	})
	for {
		update := client.next("detail-b")
		if update["_tag"] == "Exit" {
			t.Fatal("thread subscription ended before observing the follow-up")
		}
		if bytes.Contains(marshalDispatch(t, update), []byte("follow-up-m")) {
			break
		}
	}
	client.waitReply("dispatch-b", "second fixture turn complete")
	other := client.rpc("orchestration.getThreadProjection", map[string]any{"threadId": "dispatch-a"})
	if bytes.Contains(marshalDispatch(t, other), []byte("Continue the second thread")) {
		t.Fatal("follow-up leaked to the other thread")
	}
	detail := dispatchHTTP(t, origin, token, "/api/orchestration/threads/dispatch-b")
	if raw := marshalDispatch(t, detail); !bytes.Contains(raw, []byte("dispatch-b")) {
		t.Fatal("thread-specific HTTP was not routed")
	}
	t.Log("single pairing; two genuine T3 backends; two completed ACP turns; merged shell; same identity and routed follow-up after proxy restart")
}

type dispatchTestClient struct {
	t       *testing.T
	ctx     context.Context
	conn    *websocket.Conn
	pending map[string][]map[string]any
	seq     int
}

func newDispatchTestClient(t *testing.T, ctx context.Context, origin, token string) *dispatchTestClient {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/auth/websocket-ticket/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("dispatch ticket request failed")
	}
	ticket, _ := decodeDispatchResponse(t, res)["ticket"].(string)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(origin, "http")+"/ws?orchestrationProtocol=2&wsTicket="+url.QueryEscape(ticket), nil)
	if err != nil {
		t.Fatal("dispatch socket dial failed")
	}
	conn.SetReadLimit(maxWSMessage)
	return &dispatchTestClient{t: t, ctx: ctx, conn: conn, pending: map[string][]map[string]any{}}
}

func (c *dispatchTestClient) send(id any, tag string, payload map[string]any) {
	c.t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	err := writeDispatchFrame(c.ctx, c.conn, map[string]any{"_tag": "Request", "id": id, "tag": tag, "payload": payload, "headers": []any{}})
	if err != nil {
		c.t.Fatal("dispatch RPC write failed")
	}
}

func (c *dispatchTestClient) next(id any) map[string]any {
	c.t.Helper()
	wanted := requestKey(id)
	for len(c.pending[wanted]) == 0 {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.t.Fatalf("dispatch RPC %v read failed: %v", id, err)
		}
		replies, _, ok := messages(data)
		if !ok {
			c.t.Fatal("bad dispatch wire response")
		}
		for _, reply := range replies {
			if replyID, exists := reply["requestId"]; exists {
				key := requestKey(replyID)
				c.pending[key] = append(c.pending[key], reply)
				if reply["_tag"] == "Chunk" {
					_ = writeDispatchFrame(c.ctx, c.conn, map[string]any{"_tag": "Ack", "requestId": replyID})
				}
			}
		}
	}
	result := c.pending[wanted][0]
	c.pending[wanted] = c.pending[wanted][1:]
	return result
}

func (c *dispatchTestClient) result(reply map[string]any) map[string]any {
	c.t.Helper()
	exit, _ := reply["exit"].(map[string]any)
	if exit["_tag"] != "Success" {
		c.t.Fatalf("dispatch RPC %v failed: %v", reply["requestId"], exit["cause"])
	}
	result, _ := exit["value"].(map[string]any)
	return result
}

func (c *dispatchTestClient) rpc(tag string, payload map[string]any) map[string]any {
	c.t.Helper()
	c.seq++
	id := fmt.Sprintf("%s-%d", tag, c.seq)
	c.send(id, tag, payload)
	return c.result(c.next(id))
}

func (c *dispatchTestClient) waitReply(thread, message string) {
	c.t.Helper()
	for {
		p := c.rpc("orchestration.getThreadProjection", map[string]any{"threadId": thread})
		if bytes.Contains(marshalDispatch(c.t, p), []byte(message)) {
			return
		}
		select {
		case <-c.ctx.Done():
			c.t.Fatalf("thread %s did not receive %q", thread, message)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func dispatchHTTP(t *testing.T, origin, token, path string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
	req.Header.Set("X-T3-Orchestration-Protocol", "2")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("dispatch HTTP request failed")
	}
	return decodeDispatchResponse(t, res)
}

func decodeDispatchResponse(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, maxJSON)).Decode(&out) != nil {
		t.Fatalf("invalid dispatch HTTP response: status=%d", res.StatusCode)
	}
	return out
}

func marshalDispatch(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
