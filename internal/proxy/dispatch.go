package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// DispatchWorker is a dedicated, pre-provisioned T3 server. The ID must identify
// the same machine (and T3 data) after a restart.
type DispatchWorker struct {
	ID         string
	Resolve    func(context.Context) (Upstream, error)
	Descriptor func(context.Context) (map[string]any, error)
}

type dispatchState struct {
	Origin        string
	EnvironmentID string
	Workers       []string
	Threads       map[string]int
	Clients       map[string]time.Time
	OnDemand      *onDemandDisk        `json:",omitempty"`
	PeerGrants    map[string]peerGrant `json:",omitempty"`
}

type dispatchPOC struct {
	mu           sync.Mutex
	store        *state.Store
	disk         dispatchState
	workers      []DispatchWorker
	timeout      time.Duration
	sequence     atomic.Uint64
	onDemand     *DispatchOnDemand
	preparing    map[string]chan struct{}
	peerLaunches sync.Map
	demandWrites [32]sync.Mutex
}

const dispatchLabel = "t3fleet"

// NewDispatchPOC exposes a distinct environment and assigns at most one new
// thread to each worker. Workers must share a prepared workspace layout.
func NewDispatchPOC(origin string, store *state.Store, workers []DispatchWorker) (*Proxy, error) {
	if store == nil || len(workers) < 2 || len(workers) > 8 {
		return nil, errors.New("dispatch requires private state and 2–8 workers")
	}
	var ids []string
	for _, w := range workers {
		if w.ID == "" || w.Resolve == nil || slices.Contains(ids, w.ID) {
			return nil, errors.New("dispatch workers must be distinct and resolvable")
		}
		ids = append(ids, w.ID)
	}
	d := &dispatchPOC{store: store, workers: slices.Clone(workers), timeout: 2 * time.Minute}
	if err := store.Load(&d.disk); errors.Is(err, state.ErrEmpty) {
		id, err := randomToken()
		if err != nil {
			return nil, err
		}
		d.disk = dispatchState{Origin: origin, EnvironmentID: "t3fleet-" + id, Workers: ids, Threads: map[string]int{}, Clients: map[string]time.Time{}}
		if err := store.Save(d.disk); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if d.disk.Origin != origin || !slices.Equal(d.disk.Workers, ids) || d.disk.EnvironmentID == "" || d.disk.Threads == nil || d.disk.Clients == nil {
		return nil, errors.New("dispatch state must keep its original origin and ordered workers")
	}
	var taken [8]bool
	for id, index := range d.disk.Threads {
		if !validDispatchThreadID(id) || index < 0 || index >= len(workers) || taken[index] {
			return nil, errors.New("invalid saved dispatch routes")
		}
		taken[index] = true
	}
	if d.disk.OnDemand != nil {
		return nil, errors.New("on-demand state cannot be used by the fixed worker pool")
	}
	return d.proxy(origin)
}

func (d *dispatchPOC) proxy(origin string) (*Proxy, error) {
	environmentID := d.disk.EnvironmentID
	descriptor := d.workers[0].Descriptor
	if descriptor == nil {
		descriptor = func(ctx context.Context) (map[string]any, error) {
			return d.get(ctx, 0, "/.well-known/t3/environment")
		}
	}
	p, err := New(Config{
		Origin: origin, Label: dispatchLabel, Resolve: d.workers[0].Resolve,
		Authorize: d.authorize, SaveToken: d.saveToken, RequestTimeout: d.timeout,
		Descriptor: func(ctx context.Context) (map[string]any, error) {
			v, err := descriptor(ctx)
			if err == nil && v != nil {
				v = maps.Clone(v)
				v["environmentId"] = environmentID
				if capabilities, ok := v["capabilities"].(map[string]any); ok {
					capabilities = maps.Clone(capabilities)
					d.dispatchCapabilities(capabilities)
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

func (d *dispatchPOC) dispatchCapabilities(v map[string]any) {
	for _, key := range []string{"shellThreadFamilies", "shellOlderSettled", "linkedThreadRefreshes"} {
		if _, ok := v[key]; ok {
			v[key] = false
		}
	}
	if d.onDemand == nil {
		for _, key := range []string{"peerLinks", "peerLinkPairingCodes", "linkFence"} {
			v[key] = false
		}
	}
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (d *dispatchPOC) authorize(token string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Now().Before(d.disk.Clients[tokenDigest(token)])
}

func (d *dispatchPOC) saveToken(_ context.Context, token string) error {
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

func (d *dispatchPOC) worker(id string, create bool) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if index, ok := d.disk.Threads[id]; ok {
		return index, nil
	}
	if !create || !validDispatchThreadID(id) {
		return 0, errors.New("thread has no dispatch assignment")
	}
	index := len(d.disk.Threads)
	if index >= len(d.workers) {
		return 0, errors.New("dispatch pool has no unused worker")
	}
	next := d.disk
	next.Threads = maps.Clone(next.Threads)
	next.Threads[id] = index
	if err := d.store.Save(next); err != nil {
		return 0, errors.New("could not persist dispatch assignment")
	}
	d.disk = next
	return index, nil
}

func validDispatchThreadID(id string) bool {
	return id != "" && id != "." && id != ".." && len(id) <= 256 && !strings.ContainsAny(id, "/\\?#%\x00\r\n")
}

func (d *dispatchPOC) get(ctx context.Context, index int, path string) (map[string]any, error) {
	return d.getFrom(ctx, d.workers[index], path)
}

func (d *dispatchPOC) getFrom(ctx context.Context, worker DispatchWorker, path string) (map[string]any, error) {
	up, ctx, cancel, err := resolveUpstream(ctx, d.timeout, worker.Resolve)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
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
	defer resp.Body.Close()
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

func (d *dispatchPOC) shell(ctx context.Context) (map[string]any, error) {
	if d.onDemand != nil {
		return d.onDemandShell(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows := make([]map[string]any, len(d.workers))
	var wg sync.WaitGroup
	for index := range rows {
		wg.Go(func() { rows[index], _ = d.get(ctx, index, dispatchShellPath) })
	}
	wg.Wait()
	d.mu.Lock()
	routes := maps.Clone(d.disk.Threads)
	d.mu.Unlock()
	return mergeDispatchShell(rows, routes, false)
}

func mergeDispatchShell(rows []map[string]any, routes map[string]int, controllerProjects bool) (map[string]any, error) {
	result := map[string]any{"snapshotSequence": 0}
	schema, ok := rows[0]["schemaVersion"].(float64)
	if !ok {
		return nil, errors.New("dispatch shell unavailable")
	}
	for _, key := range []string{"projects", "threads", "archivedThreads"} {
		list := []any{}
		seen := map[string]bool{}
		for index, row := range rows {
			if controllerProjects && key == "projects" && index != 0 {
				break
			}
			items, ok := row[key].([]any)
			if version, valid := row["schemaVersion"].(float64); !ok || !valid || version != schema {
				return nil, errors.New("dispatch shell unavailable")
			}
			for _, item := range items {
				obj, _ := item.(map[string]any)
				id, _ := obj["id"].(string)
				if id == "" || seen[id] {
					continue
				}
				// Hide unmanaged threads rather than silently adopting them.
				if key != "projects" {
					if assigned, ok := routes[id]; !ok || assigned != index {
						continue
					}
				}
				seen[id] = true
				list = append(list, item)
			}
		}
		result[key] = list
	}
	result["schemaVersion"] = schema
	return result, nil
}

func (d *dispatchPOC) serve(p *Proxy, w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == dispatchShellPath:
		snapshot, err := d.shell(r.Context())
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
			fail(w, http.StatusNotImplemented, "route outside dispatch POC")
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
		fail(w, http.StatusNotImplemented, "route outside dispatch POC")
	}
}

type dispatchCall struct {
	cancel context.CancelFunc
	acks   chan map[string]any
}

func (d *dispatchPOC) socket(w http.ResponseWriter, r *http.Request) {
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

func (d *dispatchPOC) call(ctx context.Context, frame map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
	tag, _ := frame["tag"].(string)
	payload, _ := frame["payload"].(map[string]any)
	threadID, _ := payload["threadId"].(string)
	id := frame["id"]
	if d.onDemand != nil && tag != "orchestration.subscribeShell" {
		return d.callOnDemand(ctx, tag, frame, payload, acks, send)
	}
	switch tag {
	case "orchestration.subscribeShell":
		return d.streamShell(ctx, id, payload["requestCompletionMarker"] == true, send)
	case "projects.mutate":
		if payload["type"] != "project.create" {
			return errors.New("only project creation is supported by dispatch POC")
		}
		// Seed the same project ID/path on all dedicated workers. A retry with
		// the same command ID can reconcile a partial result; never retry here.
		var primary map[string]any
		for index := range d.workers {
			err := d.rpc(ctx, index, frame, nil, func(reply map[string]any) error {
				exit, _ := reply["exit"].(map[string]any)
				if exit["_tag"] != "Success" {
					return fmt.Errorf("worker %d rejected project creation", index+1)
				}
				if index == 0 {
					primary = reply
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		if primary == nil {
			return errors.New("project creation returned no result")
		}
		return send(primary)
	case "orchestration.launchThread", "orchestration.dispatchCommand":
		create := tag == "orchestration.launchThread" && payload["reuseExistingThread"] != true ||
			tag == "orchestration.dispatchCommand" && payload["type"] == "thread.create"
		if create && threadID == "" && tag == "orchestration.launchThread" {
			var err error
			threadID, err = randomToken()
			if err != nil {
				return errors.New("could not create thread identity")
			}
			payload["threadId"] = threadID
		}
		if tag == "orchestration.dispatchCommand" {
			command, _ := payload["type"].(string)
			if !slices.Contains([]string{"thread.create", "message.dispatch", "thread.visit", "thread.mark-unread",
				"thread.metadata.update", "thread.archive", "thread.unarchive", "thread.settle", "thread.pin", "thread.unpin", "thread.stop",
				"thread.turn.interrupt", "thread.approval.respond", "thread.user-input.respond"}, command) {
				return errors.New("command outside dispatch POC")
			}
		}
		index, err := d.worker(threadID, create)
		if err != nil {
			return err
		}
		return d.rpc(ctx, index, frame, acks, send)
	case "orchestration.subscribeThread", "orchestration.getThreadProjection", "orchestration.subscribePromptSuggestion",
		"orchestration.subscribeCommandOutput", "orchestration.getTurnItem", "orchestration.getFullThreadDiff",
		"orchestration.getNativeChildWork", "orchestration.getThreadFamily":
		index, err := d.worker(threadID, false)
		if err != nil {
			return err
		}
		return d.rpc(ctx, index, frame, acks, send)
	case "server.getConfig", "server.probe", "subscribeServerConfig", "filesystem.browse", "filesystem.getMetadata",
		"vcs.listRefs", "projects.listEntries", "projects.searchEntries", "projects.listIgnoredFiles",
		"provider.install.subscribe":
		return d.rpc(ctx, 0, frame, acks, send)
	default:
		return errors.New("RPC outside dispatch POC")
	}
}

func (d *dispatchPOC) streamShell(ctx context.Context, id any, marker bool, send func(map[string]any) error) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous []byte
	for {
		snapshot, err := d.shell(ctx)
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

// A separate upstream socket per RPC isolates request IDs and ACKs across
// workers. This intentionally trades connections for a small proof of routing.
func (d *dispatchPOC) rpc(parent context.Context, index int, frame map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
	return d.rpcTo(parent, d.workers[index], frame, acks, send)
}

func (d *dispatchPOC) rpcTo(parent context.Context, worker DispatchWorker, frame map[string]any, acks <-chan map[string]any, send func(map[string]any) error) error {
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

func (d *dispatchPOC) sanitizeConfig(tag any, reply map[string]any) {
	rewrite := func(raw any) {
		cfg, _ := raw.(map[string]any)
		stripDirect(cfg)
		if cfg != nil && d.onDemand != nil {
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
				d.dispatchCapabilities(caps)
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
			if d.onDemand != nil && (obj["type"] == "providerStatuses" || obj["type"] == "providerStatusDeltas") {
				obj["type"] = "providerStatuses"
				obj["payload"] = map[string]any{"providers": d.demandProviders()}
			}
		}
	}
}
