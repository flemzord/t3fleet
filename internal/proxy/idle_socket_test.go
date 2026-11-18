package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestSleepingSocketIgnoresBackgroundAndWakesForMessageOnce(t *testing.T) {
	var resolutions, upstreamRequests atomic.Int32
	order := make(chan string, 2)
	upstream := sourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"effect-rpc"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			values, _, ok := messages(data)
			if !ok {
				t.Error("invalid forwarded request")
				return
			}
			for _, v := range values {
				upstreamRequests.Add(1)
				order <- fmt.Sprint(v["tag"])
				payload := map[string]any{"_tag": "Exit", "requestId": v["id"], "exit": map[string]any{"_tag": "Success", "value": map[string]any{"received": v["tag"]}}}
				out, _ := json.Marshal(payload)
				if ws.Write(r.Context(), websocket.MessageText, out) != nil {
					return
				}
			}
		}
	})
	a := NewActivity(time.Now, true)
	_, s := proxyServer(t, upstream.URL, func(c *Config) {
		c.Activity = a
		c.Authorize = func(token string) bool { return token == "client" }
		c.SaveToken = func(context.Context, string) error { return nil }
		c.Session = func(context.Context) (map[string]any, error) {
			return map[string]any{"scopes": []string{"orchestration:read"}}, nil
		}
		c.Resolve = func(context.Context) (Upstream, error) {
			resolutions.Add(1)
			a.Awake()
			return Upstream{Endpoint: provider.Endpoint{URL: upstream.URL}, Token: "service-secret"}, nil
		}
	})
	// Authenticated metadata, media reconnects and shell polling never wake.
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/api/auth/session", "client", 200}, {"/api/auth/session", "foreign", 401},
		{"/api/orchestration/shell", "client", 503}, {"/api/preview-stream/a", "client", 503},
	} {
		res, _ := request(t, "GET", s.URL+tc.path, tc.token)
		if res.StatusCode != tc.status {
			t.Fatal(tc, res.StatusCode)
		}
	}
	if resolutions.Load() != 0 {
		t.Fatal("metadata woke upstream")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticket := ticketValue(t, s, "client", "rpc")
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/ws?wsTicket="+ticket, &websocket.DialOptions{Subprotocols: []string{"effect-rpc"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(data string) {
		t.Helper()
		if err := ws.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	send(`[{"_tag":"Request","id":"background","tag":"subscribeServerConfig","payload":{}},{"_tag":"Ping"}]`)
	_, data, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(data), "Pong") {
		t.Fatal("sleeping ping failed", err, string(data))
	}
	if resolutions.Load() != 0 {
		t.Fatal("subscription or ping woke upstream")
	}
	send(`{"_tag":"Request","id":"message","tag":"orchestration.dispatchCommand","payload":{"type":"message.dispatch","commandId":"one"}}`)
	seen := map[string]bool{}
	for range 2 {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if json.Unmarshal(data, &value) != nil {
			t.Fatal("bad response")
		}
		seen[fmt.Sprint(value["requestId"])] = true
	}
	if first, second := <-order, <-order; first != "subscribeServerConfig" || second != "orchestration.dispatchCommand" {
		t.Fatal("pending RPC order changed", first, second)
	}
	if !seen["background"] || !seen["message"] || resolutions.Load() != 1 || upstreamRequests.Load() != 2 {
		t.Fatal("lost or duplicated requests", seen, resolutions.Load(), upstreamRequests.Load())
	}
}

func TestIdleRPCClassificationIsConservative(t *testing.T) {
	for _, tc := range []struct {
		tag, payload string
		passive      bool
	}{
		{"subscribeServerConfig", `{}`, true}, {"orchestration.subscribeThread", `{}`, true},
		{"server.refreshProviders", `{"reloadSecrets":true}`, false}, {"server.refreshProviders", `{}`, true},
		{"server.searchAcpRegistry", `{"query":"typed"}`, false}, {"server.searchAcpRegistry", `{"query":""}`, true},
		{"vcs.listRefs", `{"query":"main","limit":1}`, false}, {"vcs.listRefs", `{"limit":10}`, true},
		{"orchestration.dispatchCommand", `{"type":"message.dispatch"}`, false},
		{"orchestration.dispatchCommand", `{"type":"thread.visit"}`, true},
		{"future.subscribeAndRunAgent", `{}`, false},
	} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
			t.Fatal(err)
		}
		if got := passiveRPC(map[string]any{"tag": tc.tag, "payload": payload}); got != tc.passive {
			t.Fatal(tc.tag, tc.payload, got)
		}
	}
}

func TestLiveIdleSubscriptionSleepsAndSameSocketWakesWithoutReplayOfWrites(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	a := NewActivity(func() time.Time { return time.Unix(0, clock.Load()) }, false)
	var resolutions, writes atomic.Int32
	acked := make(chan struct{}, 1)
	upstream := sourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			values, _, ok := messages(data)
			if !ok {
				return
			}
			for _, v := range values {
				if v["_tag"] == "Ack" {
					select {
					case acked <- struct{}{}:
					default:
					}
					continue
				}
				var reply map[string]any
				if v["tag"] == "subscribeServerConfig" {
					reply = map[string]any{"_tag": "Chunk", "requestId": v["id"], "values": []any{map[string]any{"config": map[string]any{"directEndpoints": []string{"private"}, "name": "test"}}}}
				} else {
					writes.Add(1)
					reply = map[string]any{"_tag": "Exit", "requestId": v["id"], "exit": map[string]any{"_tag": "Success", "value": nil}}
				}
				out, _ := json.Marshal(reply)
				if ws.Write(r.Context(), websocket.MessageText, out) != nil {
					return
				}
			}
		}
	})
	_, s := proxyServer(t, upstream.URL, func(c *Config) {
		c.Activity = a
		c.Authorize = func(token string) bool { return token == "client" }
		c.SaveToken = func(context.Context, string) error { return nil }
		c.Resolve = func(context.Context) (Upstream, error) {
			resolutions.Add(1)
			a.Awake()
			return Upstream{Endpoint: provider.Endpoint{URL: upstream.URL}, Token: "service-secret"}, nil
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer client"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(data string) {
		t.Helper()
		if ws.Write(ctx, websocket.MessageText, []byte(data)) != nil {
			t.Fatal("write failed")
		}
	}
	read := func() []byte {
		t.Helper()
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	send(`{"_tag":"Request","id":"sub","tag":"subscribeServerConfig","payload":{}}`)
	if data := read(); strings.Contains(string(data), "directEndpoints") {
		t.Fatal("config leak", string(data))
	}
	send(`{"_tag":"Ack","requestId":"sub"}`)
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal("live stream acknowledgement was not forwarded")
	}
	// The upstream can receive the ACK before the proxy releases its activity
	// lease. A Pong confirms the proxy has finished handling the earlier frame.
	send(`{"_tag":"Ping"}`)
	if data := read(); !strings.Contains(string(data), "Pong") {
		t.Fatal("proxy did not answer before sleep")
	}
	clock.Add(int64(time.Minute))
	if err := a.Check(time.Minute, func() (bool, error) { return false, nil }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !a.Sleeping() {
		t.Fatal("idle subscription prevented sleep")
	}
	send(`{"_tag":"Ping"}`)
	if data := read(); !strings.Contains(string(data), "Pong") {
		t.Fatal("sleep broke client socket")
	}
	if resolutions.Load() != 1 {
		t.Fatal("sleeping ping reconnected upstream")
	}
	send(`{"_tag":"Request","id":"write","tag":"orchestration.dispatchCommand","payload":{"type":"message.dispatch","commandId":"unique"}}`)
	for range 2 {
		read()
	}
	if resolutions.Load() != 2 || writes.Load() != 1 {
		t.Fatal("wake lost/duplicated useful request", resolutions.Load(), writes.Load())
	}
	// Exit must release the useful-call lease; an open subscription alone can sleep again.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		active := a.active
		a.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("completed useful RPC retained its activity lease")
		}
	}
	clock.Add(int64(time.Minute))
	if err := a.Check(time.Minute, func() (bool, error) { return false, nil }, func() error { return nil }); err != nil || !a.Sleeping() {
		t.Fatal("could not sleep again after completed work", err)
	}

}

func TestSleepingRPCBufferIsBoundedAndRequiresAuth(t *testing.T) {
	var resolutions atomic.Int32
	a := NewActivity(time.Now, true)
	_, s := proxyServer(t, "http://127.0.0.1:1", func(c *Config) {
		c.Activity = a
		c.Authorize = func(token string) bool { return token == "client" }
		c.SaveToken = func(context.Context, string) error { return nil }
		c.Resolve = func(context.Context) (Upstream, error) {
			resolutions.Add(1)
			return Upstream{}, fmt.Errorf("unexpected resolve")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if ws, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/ws", nil); err == nil {
		ws.CloseNow()
		t.Fatal("unauthorized socket accepted")
	} else if res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatal("unexpected unauthorized response", res, err)
	}
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer client"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	values := make([]map[string]any, maxIdleRequests+1)
	for i := range values {
		values[i] = map[string]any{"_tag": "Request", "id": i, "tag": "subscribeServerConfig", "payload": map[string]any{}}
	}
	data, _ := json.Marshal(values)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("oversized pending request set was accepted")
	}
	if resolutions.Load() != 0 {
		t.Fatal("buffer overflow woke machine")
	}
}

func TestIdleRPCPayloadPreservesExactJSONNumbers(t *testing.T) {
	original := []byte(`{"_tag":"Request","id":"precise","tag":"custom.action","payload":{"integer":9007199254740993,"decimal":1.234567890123456789}}`)
	values, _, ok := messages(original)
	if !ok {
		t.Fatal("valid RPC rejected")
	}
	forwarded, err := json.Marshal(values[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(forwarded), "9007199254740993") || !strings.Contains(string(forwarded), "1.234567890123456789") {
		t.Fatal("RPC payload lost numeric precision", string(forwarded))
	}
	values, _, ok = messages([]byte(`{"_tag":"Request","id":"background","tag":"vcs.listRefs","payload":{"limit":10}}`))
	if !ok || !passiveRPC(values[0]) {
		t.Fatal("number-preserving decoder broke background classification")
	}
}
