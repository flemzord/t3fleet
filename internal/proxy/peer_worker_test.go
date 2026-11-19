package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestWorkerPeerResponseJSON(t *testing.T) {
	input := "event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\r\n\r\n" +
		"event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\r\ndata: \"result\":{\"content\":[]}}\r\n\r\n"
	got := workerPeerResponseJSON([]byte(input), "text/event-stream; charset=utf-8")
	var envelope struct {
		ID int
	}
	if err := json.Unmarshal(got, &envelope); err != nil || envelope.ID != 1 {
		t.Fatalf("unexpected normalized MCP response: %s", got)
	}
}

func TestLiveWorkerNativePeer(t *testing.T) {
	origin, service, _ := startLiveT3(t, nil)
	worker := DispatchWorker{Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: service}, nil
	}}
	d := &dispatcher{timeout: 10 * time.Second}
	token, err := d.mintWorkerPeer(t.Context(), worker, "approval-required", "t3fleet worker test")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("T3-Mode-Limit", "approval-required/default")
	headers.Set("Authorization", "Bearer must-not-reach-worker")
	headers.Set("Mcp-Session-Id", "controller-session")
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t3_environment_read","arguments":{}}}`)
	body, err := d.callWorkerPeer(t.Context(), worker, token, headers, request)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			IsError           bool
			StructuredContent struct{ EnvironmentID string }
		}
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Result.IsError ||
		response.Result.StructuredContent.EnvironmentID == "" {
		t.Fatalf("unexpected worker MCP result: %s", body)
	}
	if _, err := d.callWorkerPeer(t.Context(), worker, service, headers, request); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal("worker accepted its service bearer as a native peer")
	}
	headers.Set("T3-Mode-Limit", "full-access/default")
	launch := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t3_thread_launch","arguments":{"title":"must not start","runtimeMode":"full-access"}}}`)
	body, err = d.callWorkerPeer(t.Context(), worker, token, headers, launch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("runtime_mode_escalation_denied")) {
		t.Fatalf("native worker did not enforce the granted mode: %s", body)
	}
}
