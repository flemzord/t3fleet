package proxy

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

const maxIdleRequests = 256
const maxIdleRequestBytes = 1 << 20

type socketFrame struct {
	kind websocket.MessageType
	data []byte
	err  error
}
type idleRequest struct {
	order   uint64
	raw     []byte
	sent    bool
	release func()
}

// idleSocket holds background subscriptions locally while asleep. It sends no
// invented snapshots and never acknowledges a mutation before the server does.
// The first useful RPC opens the upstream, then forwards each pending request once.
func (p *Proxy) idleSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ws" {
		if mediaPath(r.URL.Path) {
			done, ok := p.cfg.Activity.BeginPassive()
			if !ok {
				fail(w, 503, "machine is asleep")
				return
			}
			defer done()
		} else {
			done := p.cfg.Activity.Begin()
			defer done()
		}
		p.liveSocket(w, r)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: strings.FieldsFunc(r.Header.Get("Sec-WebSocket-Protocol"), func(c rune) bool { return c == ',' || c == ' ' })})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(maxWSMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	read := func(c *websocket.Conn, ctx context.Context) <-chan socketFrame {
		ch := make(chan socketFrame, 1)
		go func() {
			for {
				kind, data, err := c.Read(ctx)
				select {
				case ch <- socketFrame{kind, data, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
		return ch
	}
	down := read(client, ctx)
	var machine *websocket.Conn
	var up <-chan socketFrame
	var sleeping <-chan struct{}
	cleanup := func() {}
	tracker := &rpcRequests{tags: map[string]string{}}
	requests := map[string]*idleRequest{}
	bytesHeld := 0
	var nextOrder uint64
	defer func() {
		cleanup()
		for _, req := range requests {
			if req.release != nil {
				req.release()
			}
		}
	}()
	detach := func() {
		cleanup()
		cleanup = func() {}
		machine = nil
		up = nil
		sleeping = nil
		for _, req := range requests {
			req.sent = false
		}
	}
	connect := func() bool {
		if machine != nil && !p.cfg.Activity.Sleeping() {
			return true
		}
		detach()
		upstream, requestCtx, requestCancel, err := p.resolve(r)
		if err != nil {
			return false
		}
		tr := transport(upstream.Endpoint)
		headers := upstreamHeaders(upstream.Endpoint, upstream.Token)
		for _, name := range []string{"DPoP", "X-T3-Orchestration-Protocol", "B3", "Traceparent"} {
			if value := r.Header.Get(name); value != "" {
				headers.Set(name, value)
			}
		}
		conn, _, err := websocket.Dial(requestCtx, targetURL(upstream.Endpoint, r, true).String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, HTTPHeader: headers, Subprotocols: []string{client.Subprotocol()}})
		if err != nil {
			requestCancel()
			closeEndpoint(upstream.Endpoint)
			tr.CloseIdleConnections()
			return false
		}
		conn.SetReadLimit(maxWSMessage)
		streamCtx, streamCancel := context.WithCancel(ctx)
		signal := make(chan struct{})
		var once sync.Once
		unwatch := p.cfg.Activity.watch(func() { once.Do(func() { close(signal) }) })
		cleanup = func() {
			unwatch()
			streamCancel()
			_ = conn.CloseNow()
			requestCancel()
			closeEndpoint(upstream.Endpoint)
			tr.CloseIdleConnections()
		}
		machine = conn
		up = read(conn, streamCtx)
		sleeping = signal
		return true
	}
	flush := func() bool {
		pending := make([]*idleRequest, 0, len(requests))
		for _, req := range requests {
			if !req.sent {
				pending = append(pending, req)
			}
		}
		slices.SortFunc(pending, func(a, b *idleRequest) int { return cmp.Compare(a.order, b.order) })
		for _, req := range pending {
			if !req.sent {
				tracker.observe(req.raw)
				if err := machine.Write(ctx, websocket.MessageText, req.raw); err != nil {
					return false
				}
				req.sent = true
			}
		}
		return true
	}
	// Connecting an already-running VM is background transport, not user activity.
	if done, ok := p.cfg.Activity.BeginPassive(); ok {
		connected := connect()
		done()
		if !connected {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sleeping:
			detach()
		case frame := <-up:
			if frame.err != nil {
				if p.cfg.Activity.Sleeping() {
					detach()
					continue
				}
				return
			}
			data := frame.data
			if frame.kind == websocket.MessageText {
				data = tracker.sanitize(data)
			}
			if err := client.Write(ctx, frame.kind, data); err != nil {
				return
			}
			values, _, ok := messages(frame.data)
			if ok {
				for _, v := range values {
					if v["_tag"] == "Exit" {
						key := requestKey(v["requestId"])
						if req := requests[key]; req != nil {
							if req.release != nil {
								req.release()
							}
							bytesHeld -= len(req.raw)
							delete(requests, key)
						}
					}
				}
			}
		case frame := <-down:
			if frame.err != nil {
				return
			}
			values, _, ok := messages(frame.data)
			if !ok || frame.kind != websocket.MessageText {
				return
			}
			for _, v := range values {
				switch v["_tag"] {
				case "Ping":
					if client.Write(ctx, websocket.MessageText, []byte(`{"_tag":"Pong"}`)) != nil {
						return
					}
				case "Pong":
				case "Request":
					if v["id"] == nil {
						return
					}
					if v["tag"] == "server.probe" && p.cfg.Activity.Sleeping() {
						reply, _ := json.Marshal(map[string]any{"_tag": "Exit", "requestId": v["id"], "exit": map[string]any{"_tag": "Success", "value": map[string]any{}}})
						if client.Write(ctx, websocket.MessageText, reply) != nil {
							return
						}
						continue
					}
					key := requestKey(v["id"])
					if requests[key] != nil {
						return
					}
					raw, err := json.Marshal(v)
					if err != nil || len(requests) >= maxIdleRequests || bytesHeld+len(raw) > maxIdleRequestBytes {
						return
					}
					nextOrder++
					req := &idleRequest{raw: raw, order: nextOrder}
					requests[key] = req
					bytesHeld += len(raw)
					passive := passiveRPC(v)
					if !passive {
						req.release = p.cfg.Activity.Begin()
					}
					if machine == nil && passive {
						continue
					}
					if !passive {
						if !connect() || !flush() {
							return
						}
					} else {
						done, allowed := p.cfg.Activity.BeginPassive()
						if !allowed {
							detach()
							continue
						}
						sent := flush()
						done()
						if !sent {
							return
						}
					}
				case "Ack":
					if machine != nil {
						done, allowed := p.cfg.Activity.BeginPassive()
						if !allowed {
							detach()
							continue
						}
						raw, _ := json.Marshal(v)
						err := machine.Write(ctx, websocket.MessageText, raw)
						done()
						if err != nil {
							return
						}
					}
				case "Interrupt":
					key := requestKey(v["requestId"])
					req := requests[key]
					if req != nil && !req.sent {
						if req.release != nil {
							req.release()
						}
						bytesHeld -= len(req.raw)
						delete(requests, key)
						continue
					}
					if machine != nil {
						raw, _ := json.Marshal(v)
						if machine.Write(ctx, websocket.MessageText, raw) != nil {
							return
						}
					}
				default:
					// A new protocol control frame is not silently classified as inactivity.
					return
				}
			}
		}
	}
}
