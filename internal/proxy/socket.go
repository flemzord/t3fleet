package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func (p *Proxy) socket(w http.ResponseWriter, r *http.Request) {
	up, ctx, cancel, err := p.resolve(r)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "machine is not ready")
		return
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	headers := upstreamHeaders(up.Endpoint, up.Token)
	for _, name := range []string{"DPoP", "X-T3-Orchestration-Protocol", "B3", "Traceparent"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	machine, _, err := websocket.Dial(ctx, targetURL(up.Endpoint, r, true).String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, HTTPHeader: headers, Subprotocols: strings.FieldsFunc(r.Header.Get("Sec-WebSocket-Protocol"), func(r rune) bool { return r == ',' || r == ' ' })})
	if err != nil {
		fail(w, http.StatusBadGateway, "upstream socket unavailable")
		return
	}
	defer machine.CloseNow()
	// Host and exact Origin were checked in ServeHTTP. This flag only disables
	// the library's second (host-only) origin check, not TLS verification.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{machine.Subprotocol()}})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(maxWSMessage)
	machine.SetReadLimit(maxWSMessage)
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	tracker := &rpcRequests{tags: map[string]string{}}
	result := make(chan error, 2)
	go func() {
		result <- copySocket(sessionCtx, machine, client, func(data []byte) []byte {
			if r.URL.Path == "/ws" {
				tracker.observe(data)
			}
			return data
		})
	}()
	go func() {
		result <- copySocket(sessionCtx, client, machine, func(data []byte) []byte {
			if r.URL.Path == "/ws" {
				return tracker.sanitize(data)
			}
			return data
		})
	}()
	err = <-result
	code, reason := websocket.StatusNormalClosure, ""
	if err != nil {
		if status := websocket.CloseStatus(err); status >= 1000 && status <= 4999 && status != 1005 && status != 1006 && status != 1015 {
			code = status
			if closeErr, ok := errors.AsType[websocket.CloseError](err); ok {
				reason = closeErr.Reason
			}
		} else if !errors.Is(err, context.Canceled) {
			code = websocket.StatusTryAgainLater
			reason = "connection interrupted"
		}
	}
	// A short context keeps a peer that does not answer its close handshake from
	// retaining the endpoint. CloseNow below is the final resource guarantee.
	done := make(chan struct{}, 2)
	go func() { _ = client.Close(code, reason); done <- struct{}{} }()
	go func() { _ = machine.Close(code, reason); done <- struct{}{} }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-done:
		case <-timer.C:
			sessionCancel()
			_ = client.CloseNow()
			_ = machine.CloseNow()
			<-result
			return
		}
	}
	sessionCancel()
	_ = client.CloseNow()
	_ = machine.CloseNow()
	<-result
}

func copySocket(ctx context.Context, to, from *websocket.Conn, transform func([]byte) []byte) error {
	for {
		kind, data, err := from.Read(ctx)
		if err != nil {
			return err
		}
		if kind == websocket.MessageText {
			data = transform(data)
		}
		if err := to.Write(ctx, kind, data); err != nil {
			return err
		}
	}
}

type rpcRequests struct {
	mu   sync.Mutex
	tags map[string]string
}

func messages(data []byte) ([]map[string]any, bool, bool) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return nil, false, false
	}
	if values, ok := value.([]any); ok {
		out := make([]map[string]any, 0, len(values))
		for _, v := range values {
			item, ok := v.(map[string]any)
			if !ok {
				return nil, false, false
			}
			out = append(out, item)
		}
		return out, true, true
	}
	item, ok := value.(map[string]any)
	if !ok {
		return nil, false, false
	}
	return []map[string]any{item}, false, true
}
func requestKey(value any) string { b, _ := json.Marshal(value); return string(b) }
func (r *rpcRequests) observe(data []byte) {
	values, _, ok := messages(data)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if v["_tag"] == "Request" {
			tag, _ := v["tag"].(string)
			if tag == "subscribeServerConfig" || tag == "server.getConfig" {
				if len(r.tags) < 4096 {
					r.tags[requestKey(v["id"])] = tag
				}
			}
		}
	}
}
func (r *rpcRequests) sanitize(data []byte) []byte {
	values, array, ok := messages(data)
	if !ok {
		return data
	}
	changed := false
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		key := requestKey(v["requestId"])
		tag := r.tags[key]
		if v["_tag"] == "Chunk" && tag == "subscribeServerConfig" {
			if items, ok := v["values"].([]any); ok {
				for _, item := range items {
					if obj, ok := item.(map[string]any); ok {
						changed = stripDirect(obj["config"]) || changed
					}
				}
			}
		}
		if v["_tag"] == "Exit" {
			delete(r.tags, key)
			if tag == "server.getConfig" {
				if exit, ok := v["exit"].(map[string]any); ok && exit["_tag"] == "Success" {
					changed = stripDirect(exit["value"]) || changed
				}
			}
		}
	}
	if !changed {
		return data
	}
	var value any = values
	if !array {
		value = values[0]
	}
	out, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return out
}
func stripDirect(value any) bool {
	if obj, ok := value.(map[string]any); ok {
		if _, ok := obj["directEndpoints"]; ok {
			delete(obj, "directEndpoints")
			return true
		}
	}
	return false
}
