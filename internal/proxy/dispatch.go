package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/internal/state"
)

// DispatchWorker resolves one T3 server. The ID must identify
// the same machine (and T3 data) after a restart.
type DispatchWorker struct {
	ID      string
	Resolve func(context.Context) (Upstream, error)
	// ResolvePassive may only connect without waking a machine. Nil means no passive transport.
	ResolvePassive func(context.Context) (Upstream, error)
	Descriptor     func(context.Context) (map[string]any, error)
}

type dispatchState struct {
	Origin        string
	EnvironmentID string
	Clients       map[string]time.Time
	OnDemand      *onDemandDisk        `json:",omitempty"`
	PeerGrants    map[string]peerGrant `json:",omitempty"`
}

type dispatcher struct {
	mu           sync.Mutex
	store        *state.Store
	disk         dispatchState
	timeout      time.Duration
	sequence     atomic.Uint64
	onDemand     *DispatchOnDemand
	preparing    map[string]chan struct{}
	peerLaunches sync.Map
	demandWrites [32]sync.Mutex
}

const dispatchLabel = "t3fleet"

func (d *dispatcher) proxy(origin string) (*Proxy, error) {
	environmentID := d.disk.EnvironmentID
	descriptor := d.onDemand.Controller.Descriptor
	if descriptor == nil {
		descriptor = func(ctx context.Context) (map[string]any, error) {
			return d.getFrom(ctx, d.onDemand.Controller, "/.well-known/t3/environment")
		}
	}
	p, err := New(Config{
		Origin: origin, Label: dispatchLabel, Resolve: d.onDemand.Controller.Resolve,
		Authorize: d.authorize, SaveToken: d.saveToken, RequestTimeout: d.timeout,
		Descriptor: func(ctx context.Context) (map[string]any, error) {
			v, err := descriptor(ctx)
			if err == nil && v != nil {
				v = maps.Clone(v)
				v["environmentId"] = environmentID
				if capabilities, ok := v["capabilities"].(map[string]any); ok {
					capabilities = maps.Clone(capabilities)
					dispatchCapabilities(capabilities)
					v["capabilities"] = capabilities
				}
			}
			return v, err
		},
	})
	if err != nil {
		return nil, err
	}
	p.dispatch = d
	return p, nil
}

func dispatchCapabilities(v map[string]any) {
	// The controller runs in desktop mode, but the fleet endpoint cannot
	// update its binary, stage releases, or switch Desktop channels.
	for _, key := range []string{"serverSelfUpdate", "serverSelfUpdateProgress", "serverUpdateStaging",
		"serverUpdateThreadContinuation", "desktopAppUpdate", "desktopAppChannelSwitch", "serverInstallation", "npmReleaseLookup"} {
		delete(v, key)
	}
	for _, key := range []string{"shellThreadFamilies", "shellOlderSettled", "linkedThreadRefreshes"} {
		if _, ok := v[key]; ok {
			v[key] = false
		}
	}
}

// Desktop offers a manual install command when self-update is absent. Tell
// it updates are managed, preserving any policy the controller already has.
func dispatchUpdatePolicy(cfg map[string]any) {
	policy, _ := cfg["managedPolicy"].(map[string]any)
	if policy == nil {
		policy = map[string]any{"organization": dispatchLabel, "lockedSettings": []string{}}
		cfg["managedPolicy"] = policy
	}
	limits, _ := policy["limits"].(map[string]any)
	if limits == nil {
		limits = map[string]any{}
		policy["limits"] = limits
	}
	updates, _ := limits["updates"].(map[string]any)
	if updates == nil {
		updates = map[string]any{}
		limits["updates"] = updates
	}
	updates["mode"] = "off"
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (d *dispatcher) authorize(token string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Now().Before(d.disk.Clients[tokenDigest(token)])
}

func (d *dispatcher) saveToken(_ context.Context, token string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.disk
	next.Clients = maps.Clone(next.Clients)
	now := time.Now()
	maps.DeleteFunc(next.Clients, func(_ string, expiry time.Time) bool { return !now.Before(expiry) })
	next.Clients[tokenDigest(token)] = now.Add(clientLifetime)
	if err := d.store.Save(next); err != nil {
		return err
	}
	d.disk = next
	return nil
}

func validDispatchThreadID(id string) bool {
	return id != "" && id != "." && id != ".." && len(id) <= 256 && !strings.ContainsAny(id, "/\\?#%\x00\r\n")
}

func (d *dispatcher) getFrom(ctx context.Context, worker DispatchWorker, path string) (map[string]any, error) {
	up, ctx, cancel, err := resolveUpstream(ctx, d.timeout, worker.Resolve)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	return fetchSnapshot(ctx, up, path)
}

// Reuse the current action's transport. The caller owns the endpoint lifetime.
func fetchSnapshot(ctx context.Context, up Upstream, path string) (map[string]any, error) {
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	rel := &http.Request{URL: &url.URL{Path: path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL(up.Endpoint, rel, false).String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = upstreamHeaders(up.Endpoint, up.Token)
	req.Header.Set("X-T3-Orchestration-Protocol", "2")
	client := &http.Client{Transport: tr, CheckRedirect: rejectRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("dispatch worker rejected HTTP request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSON+1))
	var obj map[string]any
	if err != nil || len(data) > maxJSON || json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, errors.New("invalid dispatch worker response")
	}
	return obj, nil
}

func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

const dispatchShellPath = "/api/orchestration/shell"

func (d *dispatcher) serve(p *Proxy, w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == dispatchShellPath:
		snapshot, err := d.onDemandShell(r.Context())
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "dispatch shell unavailable")
			return
		}
		writeJSON(w, snapshot)
	case r.Method == http.MethodGet && r.URL.Path == "/ws" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		d.socket(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/orchestration/threads/"):
		tail := strings.TrimPrefix(r.URL.Path, "/api/orchestration/threads/")
		id, suffix, _ := strings.Cut(tail, "/")
		if !slices.Contains([]string{"", "transcript", "bounded", "history"}, suffix) {
			fail(w, http.StatusNotImplemented, "route is not supported by the fleet")
			return
		}
		worker, err := d.threadWorker(id)
		if err != nil {
			fail(w, http.StatusNotFound, "thread has no dispatch assignment")
			return
		}
		relay := Proxy{origin: p.origin, cfg: Config{Resolve: worker.Resolve, RequestTimeout: d.timeout}}
		relay.relay(w, r)
	default:
		fail(w, http.StatusNotImplemented, "route is not supported by the fleet")
	}
}

type dispatchCall struct {
	cancel context.CancelFunc
	acks   chan map[string]any
}

func (d *dispatcher) socket(w http.ResponseWriter, r *http.Request) {
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(maxWSMessage)
	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	var mu sync.Mutex
	calls := map[string]*dispatchCall{}
	send := func(frame map[string]any) error {
		writeCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		err := writeDispatchFrame(writeCtx, client, frame)
		if err != nil {
			cancel()
		}
		return err
	}
	for {
		kind, data, err := client.Read(ctx)
		if err != nil || kind != websocket.MessageText {
			return
		}
		frames, _, ok := messages(data)
		if !ok {
			return
		}
		for _, frame := range frames {
			switch frame["_tag"] {
			case "Ping":
				if send(map[string]any{"_tag": "Pong"}) != nil {
					return
				}
			case "Ack", "Interrupt":
				mu.Lock()
				call := calls[requestKey(frame["requestId"])]
				mu.Unlock()
				if call != nil {
					if frame["_tag"] == "Interrupt" {
						call.cancel()
					} else {
						select {
						case call.acks <- frame:
						default:
						}
					}
				}
			case "Request":
				id := frame["id"]
				// Effect RPC uses numeric IDs in Desktop and also accepts
				// strings. Keep the wire type for responses and stream ACKs.
				switch value := id.(type) {
				case json.Number:
				case string:
					if value == "" {
						return
					}
				default:
					return
				}
				key := requestKey(id)
				mu.Lock()
				if _, exists := calls[key]; exists || len(calls) >= 128 {
					mu.Unlock()
					return
				}
				callCtx, stop := context.WithCancel(ctx)
				call := &dispatchCall{cancel: stop, acks: make(chan map[string]any, 32)}
				calls[key] = call
				mu.Unlock()
				wg.Go(func() {
					defer stop()
					defer func() {
						mu.Lock()
						delete(calls, key)
						mu.Unlock()
					}()
					if err := d.call(callCtx, frame, call.acks, send); err != nil && callCtx.Err() == nil {
						_ = send(dispatchFailure(id, frame, err.Error()))
					}
				})
			case "Eof":
				return
			default:
				return
			}
		}
	}
}

func dispatchFailure(id any, frame map[string]any, message string) map[string]any {
	cause := map[string]any{"_tag": "Die", "defect": message}
	switch frame["tag"] {
	case "projectClone.start":
		cause = map[string]any{"_tag": "Fail", "error": map[string]any{"_tag": "OrchestrationDispatchCommandError", "message": message}}
	case "orchestration.launchThread":
		payload, _ := frame["payload"].(map[string]any)
		cmd, _ := payload["commandId"].(string)
		project, _ := payload["projectId"].(string)
		if cmd != "" && project != "" {
			cause = map[string]any{"_tag": "Fail", "error": map[string]any{"_tag": "OrchestrationV2ThreadLaunchError", "message": message, "commandId": cmd, "projectId": project}}
		}
	}
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{
		"_tag": "Failure", "cause": []any{cause},
	}}
}

func writeDispatchFrame(ctx context.Context, conn *websocket.Conn, frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(data) > maxWSMessage {
		return errors.New("dispatch frame too large")
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func (d *dispatcher) call(ctx context.Context, frame map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
	tag, _ := frame["tag"].(string)
	payload, _ := frame["payload"].(map[string]any)
	if tag == "orchestration.subscribeShell" {
		return d.streamShell(ctx, frame["id"], payload["requestCompletionMarker"] == true, send)
	}
	return d.callOnDemand(ctx, tag, frame, payload, acks, send)
}

func (d *dispatcher) streamShell(ctx context.Context, id any, marker bool, send func(map[string]any) error) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous []byte
	for {
		snapshot, err := d.onDemandShell(ctx)
		if err == nil {
			data, err := json.Marshal(snapshot)
			if err != nil {
				return errors.New("could not encode dispatch shell")
			}
			if !bytes.Equal(data, previous) {
				first := previous == nil
				previous = data
				snapshot["snapshotSequence"] = d.sequence.Add(1)
				items := []any{map[string]any{"kind": "snapshot", "snapshot": snapshot}}
				if first && marker {
					items = append(items, map[string]any{"kind": "synchronized"})
				}
				if err := send(map[string]any{"_tag": "Chunk", "requestId": id, "values": items}); err != nil {
					return err
				}
			}
		} else if previous == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Open a separate upstream socket per RPC to isolate request IDs and acknowledgments.
func (d *dispatcher) rpcTo(parent context.Context, worker DispatchWorker, frame map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
	up, ctx, cancel, err := resolveUpstream(parent, d.timeout, worker.Resolve)
	if err != nil {
		return err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	dialCtx, stopDial := context.WithTimeout(ctx, 15*time.Second)
	socket, _, err := websocket.Dial(dialCtx,
		targetURL(up.Endpoint, &http.Request{URL: &url.URL{Path: "/ws"}}, true).String(),
		&websocket.DialOptions{HTTPHeader: upstreamHeaders(up.Endpoint, up.Token),
			HTTPClient: &http.Client{Transport: tr, CheckRedirect: rejectRedirect}})
	stopDial()
	if err != nil {
		return errors.New("dispatch worker socket unavailable")
	}
	defer socket.CloseNow()
	socket.SetReadLimit(maxWSMessage)
	if err := writeDispatchFrame(ctx, socket, frame); err != nil {
		return errors.New("dispatch request delivery uncertain")
	}
	if acks != nil {
		ended := make(chan struct{})
		defer func() { cancel(); <-ended }()
		go func() {
			defer close(ended)
			for {
				select {
				case <-ctx.Done():
					return
				case ack := <-acks:
					if writeDispatchFrame(ctx, socket, ack) != nil {
						_ = socket.CloseNow()
						return
					}
				}
			}
		}()
	}
	key := requestKey(frame["id"])
	for {
		kind, data, err := socket.Read(ctx)
		if err != nil {
			return errors.New("dispatch worker disconnected; request may have reached it")
		}
		replies, _, ok := messages(data)
		if !ok || kind != websocket.MessageText {
			return errors.New("unsupported dispatch response")
		}
		for _, reply := range replies {
			if requestKey(reply["requestId"]) != key {
				continue
			}
			if reply["_tag"] == "Exit" {
				if err := d.observeThreadRPC(ctx, up, frame, reply); err != nil {
					return err
				}
			}
			d.sanitizeConfig(frame["tag"], reply)
			if err := send(reply); err != nil {
				return err
			}
			if reply["_tag"] == "Exit" {
				return nil
			}
		}
	}
}

func (d *dispatcher) sanitizeConfig(tag any, reply map[string]any) {
	rewrite := func(raw any) {
		cfg, _ := raw.(map[string]any)
		stripDirect(cfg)
		if cfg != nil {
			dispatchUpdatePolicy(cfg)
			cfg["providers"] = d.demandProviders()
			cfg["shellOlderSettled"] = false
			cfg["shellThreadFamilies"] = false
			delete(cfg, "remoteOpenTargets")
		}
		if descriptor, ok := cfg["environment"].(map[string]any); ok {
			d.mu.Lock()
			descriptor["environmentId"] = d.disk.EnvironmentID
			d.mu.Unlock()
			descriptor["label"] = dispatchLabel
			if caps, ok := descriptor["capabilities"].(map[string]any); ok {
				dispatchCapabilities(caps)
			}
		}
	}
	switch tag {
	case "server.getConfig":
		exit, _ := reply["exit"].(map[string]any)
		rewrite(exit["value"])
	case "subscribeServerConfig":
		items, _ := reply["values"].([]any)
		for _, item := range items {
			obj, _ := item.(map[string]any)
			rewrite(obj["config"])
			if obj["type"] == "providerStatuses" || obj["type"] == "providerStatusDeltas" {
				obj["type"] = "providerStatuses"
				obj["payload"] = map[string]any{"providers": d.demandProviders()}
			}
		}
	}
}
