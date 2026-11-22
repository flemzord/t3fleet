package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

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

type dispatchTestClient struct {
	t       *testing.T
	ctx     context.Context
	conn    *websocket.Conn
	pending map[string][]map[string]any
	seq     int
}

func newDispatchTestClient(t *testing.T, ctx context.Context, origin, token string) *dispatchTestClient {
	t.Helper()
	return newDispatchTestClientHTTP(t, ctx, http.DefaultClient, origin, token)
}

func newDispatchTestClientHTTP(t *testing.T, ctx context.Context, httpClient *http.Client, origin, token string) *dispatchTestClient {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/auth/websocket-ticket/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal("dispatch ticket request failed")
	}
	ticket, _ := decodeDispatchResponse(t, res)["ticket"].(string)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(origin, "http")+"/ws?orchestrationProtocol=2&wsTicket="+url.QueryEscape(ticket),
		&websocket.DialOptions{HTTPClient: httpClient})
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
