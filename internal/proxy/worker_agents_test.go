package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// fakeSettingsWorker answers server.getConfig with its stored provider
// instances and records each server.updateSettings patch.
func fakeSettingsWorker(t *testing.T, instances map[string]any) (DispatchWorker, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var patches []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal(data, &request) != nil {
			return
		}
		var value any
		switch request["tag"] {
		case "server.getConfig":
			mu.Lock()
			value = map[string]any{"providers": []any{}, "settings": map[string]any{"providerInstances": instances}}
			mu.Unlock()
		case "server.updateSettings":
			payload, _ := request["payload"].(map[string]any)
			patch, _ := payload["patch"].(map[string]any)
			mu.Lock()
			patches = append(patches, patch)
			instances, _ = patch["providerInstances"].(map[string]any)
			mu.Unlock()
			value = map[string]any{}
		}
		reply, _ := json.Marshal(map[string]any{"_tag": "Exit", "requestId": request["id"], "exit": map[string]any{"_tag": "Success", "value": value}})
		_ = conn.Write(r.Context(), websocket.MessageText, reply)
	}))
	t.Cleanup(server.Close)
	up := Upstream{Endpoint: provider.Endpoint{URL: server.URL}, Token: "worker-token"}
	worker := DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return up, nil }}
	return worker, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return patches }
}

func TestEnableWorkerAgentsMergesProviderInstances(t *testing.T) {
	t.Parallel()
	worker, patches := fakeSettingsWorker(t, map[string]any{
		"codex": map[string]any{"driver": "codex", "accentColor": "#25eb9f"},
		"pi":    map[string]any{"driver": "pi", "enabled": false, "binaryPath": "/opt/pi"},
		"grok":  map[string]any{"driver": "grok", "enabled": false},
	})
	d := &dispatcher{timeout: 10 * time.Second, onDemand: &DispatchOnDemand{Agents: []DispatchAgent{
		{Driver: "codex", Models: []string{"gpt-5.4"}},
		{Driver: "pi", Models: []string{"ai_relay/gpt-6.2-sol"}},
	}}}
	if err := d.enableWorkerAgents(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	sent := patches()
	if len(sent) != 1 {
		t.Fatalf("expected one settings patch, got %d", len(sent))
	}
	instances, _ := sent[0]["providerInstances"].(map[string]any)
	codex, _ := instances["codex"].(map[string]any)
	pi, _ := instances["pi"].(map[string]any)
	grok, _ := instances["grok"].(map[string]any)
	if codex["enabled"] != true || codex["accentColor"] != "#25eb9f" || pi["enabled"] != true || pi["binaryPath"] != "/opt/pi" || grok["enabled"] != false {
		t.Fatalf("patch did not enable declared agents and keep other settings: %v", instances)
	}
	if err := d.enableWorkerAgents(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	if len(patches()) != 1 {
		t.Fatal("already enabled agents were patched again")
	}
}

func TestEnabledAgentInstancesReplacesMismatchedDriver(t *testing.T) {
	t.Parallel()
	current := map[string]any{"pi": map[string]any{"driver": "codex", "enabled": true}}
	instances, changed := enabledAgentInstances(current, []DispatchAgent{{Driver: "pi", Models: []string{"default"}}})
	pi, _ := instances["pi"].(map[string]any)
	if !changed || pi["driver"] != "pi" || pi["enabled"] != true {
		t.Fatalf("mismatched instance not replaced: %v", instances)
	}
	if original, _ := current["pi"].(map[string]any); original["driver"] != "codex" {
		t.Fatal("input settings were modified")
	}
}

func TestLiveEnableWorkerAgents(t *testing.T) {
	origin, token, _ := startLiveT3(t, nil)
	up := Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: token}
	worker := DispatchWorker{Resolve: func(context.Context) (Upstream, error) { return up, nil }}
	d := &dispatcher{timeout: 30 * time.Second, onDemand: &DispatchOnDemand{Agents: []DispatchAgent{
		{Driver: "codex", Models: []string{"gpt-5.4"}},
		{Driver: "pi", Models: []string{"default"}},
	}}}
	if err := d.enableWorkerAgents(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	var instances map[string]any
	read := map[string]any{"_tag": "Request", "id": "check", "tag": "server.getConfig", "headers": []any{}, "payload": map[string]any{}}
	if err := d.rpcTo(t.Context(), worker, read, nil, func(reply map[string]any) error {
		exit, _ := reply["exit"].(map[string]any)
		value, _ := exit["value"].(map[string]any)
		settings, _ := value["settings"].(map[string]any)
		instances, _ = settings["providerInstances"].(map[string]any)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pi, _ := instances["pi"].(map[string]any)
	if pi["driver"] != "pi" || pi["enabled"] != true {
		t.Fatalf("genuine T3 did not keep Pi enabled: %v", instances)
	}
}
