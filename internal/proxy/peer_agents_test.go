package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerCapabilitiesListDeclaredAgents(t *testing.T) {
	t.Parallel()
	d := &dispatcher{onDemand: &DispatchOnDemand{Agents: []DispatchAgent{
		{Driver: "claudeAgent", Models: []string{"claude-opus-5-5"}},
		{Driver: "pi", Models: []string{"deepseek/v4", "glm-5"}},
	}}}
	reply := map[string]any{"result": map[string]any{"structuredContent": map[string]any{
		"providers": []any{map[string]any{"providerInstanceId": "codex"}},
		"features":  map[string]any{"appOwnedSubagents": true, "batchThreadCreation": true, "scheduledTasks": true, "maxBatchThreads": 4},
	}}}
	recorder := httptest.NewRecorder()
	d.writePeerResult(recorder, reply, "orchestrator_capabilities")
	var got struct {
		Result struct {
			StructuredContent struct {
				Providers []struct {
					ID              string `json:"providerInstanceId"`
					Driver          string `json:"driverKind"`
					Name            string `json:"displayName"`
					Models          []struct{ ID string }
					CanRunChildTask bool
				}
				Features map[string]any
			}
		}
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	providers := got.Result.StructuredContent.Providers
	if len(providers) != 2 || providers[0].ID != "claudeAgent" || providers[0].Driver != "claudeAgent" || providers[0].Name != "Claude" ||
		providers[1].ID != "pi" || providers[1].Name != "Pi" || len(providers[1].Models) != 2 || providers[1].Models[1].ID != "glm-5" {
		t.Fatalf("capabilities do not list the declared agents: %s", recorder.Body)
	}
	if providers[1].CanRunChildTask || got.Result.StructuredContent.Features["appOwnedSubagents"] != false {
		t.Fatalf("capabilities enabled subagents: %s", recorder.Body)
	}
}

func TestPeerLaunchRejectsUndeclaredAgent(t *testing.T) {
	t.Parallel()
	d := &dispatcher{onDemand: &DispatchOnDemand{Agents: []DispatchAgent{
		{Driver: "codex", Models: []string{"gpt-5.4"}},
		{Driver: "pi", Models: []string{"deepseek/v4"}},
	}}}
	for _, selection := range []map[string]any{
		{"instanceId": "claudeAgent", "model": "claude-opus-5-5"},
		{"instanceId": "pi", "model": "gpt-5.4"},
		{"instanceId": "codex", "model": "deepseek/v4"},
		{"provider": "pi", "model": "other"},
		{"model": "gpt-5.4"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request.Header.Set("T3-Mode-Limit", "full-access/default")
		recorder := httptest.NewRecorder()
		d.launchPeerThread(recorder, request, peerRequest{ID: json.RawMessage("1")}, peerTool{Name: "t3_thread_launch", Arguments: map[string]any{
			"projectId": "demo", "title": "Council", "message": "Review", "modelSelection": selection,
		}}, "key", peerGrant{Access: "full-access"})
		if !strings.Contains(recorder.Body.String(), "invalid_request") || !strings.Contains(recorder.Body.String(), "orchestrator_capabilities") {
			t.Fatalf("selection %v was not rejected: %s", selection, recorder.Body)
		}
	}
}

func TestDispatchAgentOffers(t *testing.T) {
	t.Parallel()
	declared := &DispatchOnDemand{Agents: []DispatchAgent{{Driver: "pi", Models: []string{"deepseek/v4"}}, {Driver: "claudeAgent", Models: []string{"claude-opus-5-5"}}}}
	if !declared.offers("pi", "deepseek/v4") || !declared.offers("claudeAgent", "claude-opus-5-5") || declared.offers("codex", defaultDispatchModel) {
		t.Fatal("declared catalog not enforced")
	}
	legacy := &DispatchOnDemand{}
	if !legacy.offers("codex", defaultDispatchModel) || legacy.offers("pi", defaultDispatchModel) {
		t.Fatal("empty catalog does not default to the Codex model")
	}
	if first := declared.agents()[0]; first.Driver != "pi" || first.Models[0] != "deepseek/v4" {
		t.Fatal("first declared model is not the launch default")
	}
}
