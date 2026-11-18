package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

type peerGrant struct {
	SessionID string
	Access    string
	ExpiresAt time.Time
}

type peerRoute struct {
	Grant       string
	WorkerToken string
}

func peerModeRank(mode string) int {
	switch mode {
	case "approval-required":
		return 1
	case "auto-accept-edits":
		return 2
	case "auto":
		return 3
	case "full-access":
		return 4
	default:
		return 0
	}
}

func (d *dispatchPOC) savePeerGrant(token, access string, expiresAt time.Time) error {
	claims, ok := parsePeerNativeSession(token)
	if !ok || (access != "read-only" && peerModeRank(access) == 0) || !time.Now().Before(expiresAt) {
		return errors.New("invalid native peer grant")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.disk
	next.PeerGrants = make(map[string]peerGrant)
	now := time.Now()
	for key, grant := range d.disk.PeerGrants {
		if now.Before(grant.ExpiresAt) {
			next.PeerGrants[key] = grant
		}
	}
	next.PeerGrants[tokenDigest(token)] = peerGrant{claims.SessionID, access, expiresAt}
	if err := d.store.Save(next); err != nil {
		return errors.New("could not persist the peer grant")
	}
	d.disk = next
	return nil
}

func (d *dispatchPOC) hasPeerSession(sid string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, grant := range d.disk.PeerGrants {
		if grant.SessionID == sid {
			return true
		}
	}
	return false
}

type peerRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type peerTool struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (d *dispatchPOC) controllerMCPRequest(r *http.Request, body []byte) (*http.Response, func(), error) {
	up, ctx, cancel, err := resolveUpstream(r.Context(), d.timeout, d.workers[0].Resolve)
	if err != nil {
		return nil, nil, err
	}
	tr := transport(up.Endpoint)
	cleanup := func() { tr.CloseIdleConnections(); closeEndpoint(up.Endpoint); cancel() }
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL(up.Endpoint, r, false).String(), bytes.NewReader(body))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	req.Host = r.Host
	req.Header = up.Endpoint.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	// The controller must see the incoming peer bearer, never its service bearer.
	for _, key := range []string{"Authorization", "Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version", "T3-Mode-Limit", "T3-Sender-Thread"} {
		req.Header.Del(key)
		for _, value := range r.Header.Values(key) {
			req.Header.Add(key, value)
		}
	}
	if strings.HasPrefix(d.diskOrigin(), "https:") {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return res, func() { res.Body.Close(); cleanup() }, nil
}

func (d *dispatchPOC) diskOrigin() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disk.Origin
}

func relayPeerMCPResponse(w http.ResponseWriter, res *http.Response) {
	for _, key := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "WWW-Authenticate"} {
		for _, value := range res.Header.Values(key) {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(res.Body, maxJSON))
}

func peerToolError(w http.ResponseWriter, id json.RawMessage, code, message string) {
	content, _ := json.Marshal(map[string]string{"code": code, "message": message})
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"isError": true, "content": []any{map[string]string{"type": "text", "text": string(content)}},
	}})
}

func (d *dispatchPOC) servePeerMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Origin") != "" || r.URL.RawQuery != "" {
		fail(w, http.StatusForbidden, "unsupported peer MCP request")
		return
	}
	auth := strings.Fields(r.Header.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
		fail(w, 401, "peer grant required")
		return
	}
	key := tokenDigest(auth[1])
	d.mu.Lock()
	grant, ok := d.disk.PeerGrants[key]
	d.mu.Unlock()
	if !ok || !time.Now().Before(grant.ExpiresAt) {
		fail(w, 401, "peer grant is missing or expired")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	var request peerRequest
	if err != nil || len(body) > maxJSON || json.Unmarshal(body, &request) != nil || request.JSONRPC != "2.0" {
		fail(w, 400, "invalid peer MCP request")
		return
	}
	if request.Method != "tools/call" {
		switch request.Method {
		case "initialize", "notifications/initialized", "ping", "tools/list":
			body, _ := json.Marshal(request)
			res, closeRes, err := d.controllerMCPRequest(r, body)
			if err != nil {
				fail(w, 502, "peer controller unavailable")
				return
			}
			defer closeRes()
			relayPeerMCPResponse(w, res)
		default:
			fail(w, 400, "unsupported peer MCP method")
		}
		return
	}
	var tool peerTool
	if len(request.ID) == 0 || json.Unmarshal(request.Params, &tool) != nil || tool.Arguments == nil {
		fail(w, 400, "invalid peer tool call")
		return
	}
	// Ask the native controller to admit this exact credential and transport
	// session on every request. This also applies revocation and mode limits.
	ping, _ := json.Marshal(peerRequest{JSONRPC: "2.0", ID: request.ID, Method: "ping"})
	check, closeCheck, err := d.controllerMCPRequest(r, ping)
	if err != nil {
		fail(w, 502, "peer controller unavailable")
		return
	}
	if check.StatusCode != http.StatusOK {
		defer closeCheck()
		relayPeerMCPResponse(w, check)
		return
	}
	pingErr := checkNativePeerPing(check.Body)
	closeCheck()
	if pingErr != nil {
		fail(w, 502, "peer controller rejected the MCP session")
		return
	}

	switch tool.Name {
	case "t3_environment_read", "t3_project_list", "orchestrator_capabilities":
		request.Params, _ = json.Marshal(tool)
		body, _ := json.Marshal(request)
		res, closeRes, err := d.controllerMCPRequest(r, body)
		if err != nil {
			fail(w, 502, "peer controller unavailable")
			return
		}
		defer closeRes()
		if res.StatusCode != 200 {
			relayPeerMCPResponse(w, res)
			return
		}
		d.writePeerToolResult(w, res.Body, tool.Name)
	case "t3_thread_launch":
		d.launchPeerThread(w, r, request, tool, key, grant)
	case "t3_thread_read", "t3_thread_wait", "t3_thread_send", "t3_thread_interrupt", "t3_thread_organize":
		d.callPeerThread(w, r, request, tool, key, grant)
	default:
		peerToolError(w, request.ID, "capability_denied", "This tool is unavailable over the t3fleet link.")
	}
}

func checkNativePeerPing(body io.Reader) error {
	var result struct {
		Result json.RawMessage `json:"result"`
	}
	data, err := io.ReadAll(io.LimitReader(body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return errors.New("invalid MCP response")
	}
	if json.Unmarshal(peerResponseJSON(data), &result) != nil || len(result.Result) == 0 || bytes.Equal(result.Result, []byte("null")) {
		return errors.New("MCP session did not answer ping")
	}
	return nil
}

func peerResponseJSON(b []byte) []byte {
	if trimmed := bytes.TrimSpace(b); bytes.HasPrefix(trimmed, []byte("{")) {
		return trimmed
	}
	return workerPeerResponseJSON(b, "text/event-stream")
}

func (d *dispatchPOC) launchPeerThread(w http.ResponseWriter, r *http.Request, request peerRequest, tool peerTool, key string, grant peerGrant) {
	failTool := func(code, message string) { peerToolError(w, request.ID, code, message) }
	if grant.Access == "read-only" {
		failTool("capability_denied", "This link is read-only.")
		return
	}
	limit, interaction, ok := strings.Cut(r.Header.Get("T3-Mode-Limit"), "/")
	if !ok || peerModeRank(limit) == 0 || (interaction != "default" && interaction != "plan") {
		failTool("capability_denied", "A linked launch needs the caller's mode limit.")
		return
	}
	if asked, _ := tool.Arguments["runtimeMode"].(string); asked != "" &&
		(peerModeRank(asked) == 0 || peerModeRank(asked) > min(peerModeRank(limit), peerModeRank(grant.Access))) {
		failTool("capability_denied", "The requested mode exceeds the link or caller limit.")
		return
	}
	projectID, _ := tool.Arguments["projectId"].(string)
	title, _ := tool.Arguments["title"].(string)
	prompt, _ := tool.Arguments["message"].(string)
	requestKey, _ := tool.Arguments["clientRequestId"].(string)
	strategy, _ := tool.Arguments["workspaceStrategy"].(map[string]any)
	attachments, _ := tool.Arguments["attachments"].([]any)
	if !validDispatchThreadID(projectID) || strings.TrimSpace(title) == "" || len(title) > 512 || strings.TrimSpace(prompt) == "" ||
		len(attachments) != 0 || tool.Arguments["scratch"] == true || (strategy != nil && strategy["type"] != "root" && strategy["type"] != "worktree") ||
		(interaction == "plan" && tool.Arguments["interactionMode"] == "default") {
		failTool("invalid_request", "Pass a registered project, title and text message in a root checkout or new worktree.")
		return
	}
	model := d.peerModel()
	if requested, _ := tool.Arguments["modelSelection"].(map[string]any); requested != nil {
		instance := requested["instanceId"]
		if instance == nil {
			instance = requested["provider"]
		}
		if requested["model"] != model || instance != "codex" {
			failTool("invalid_request", "This t3fleet environment runs one configured Codex model.")
			return
		}
	} else {
		tool.Arguments["modelSelection"] = map[string]string{"instanceId": "codex", "model": model}
	}
	if requestKey == "" {
		var err error
		requestKey, err = randomToken()
		if err != nil {
			failTool("orchestration_error", "Could not create the launch key.")
			return
		}
	}
	if len(requestKey) > 512 {
		failTool("invalid_request", "The launch key is too long.")
		return
	}
	slot := "peer-" + demandKey(key, requestKey)
	// One slot across concurrent MCP sessions and across daemon restarts.
	done := make(chan struct{})
	for {
		previous, exists := d.peerLaunches.LoadOrStore(slot, done)
		if !exists {
			break
		}
		select {
		case <-previous.(chan struct{}):
		case <-r.Context().Done():
			return
		}
	}
	defer func() { d.peerLaunches.Delete(slot); close(done) }()
	d.mu.Lock()
	id := d.disk.OnDemand.PeerThreads[slot]
	if id == "" {
		id = slot
	}
	route := d.disk.OnDemand.Routes[id]
	d.mu.Unlock()
	if route.ProjectID != "" && (route.ProjectID != projectID || (route.Peer != nil && route.Peer.Grant != key)) {
		failTool("invalid_request", "A launch retry must use the original project and link.")
		return
	}
	worker, err := d.prepareBoat(r.Context(), id, projectID)
	if err != nil {
		failTool("orchestration_error", err.Error())
		return
	}
	d.mu.Lock()
	route = d.disk.OnDemand.Routes[id]
	d.mu.Unlock()
	if route.Peer == nil {
		token, err := d.mintWorkerPeer(r.Context(), worker, grant.Access, "t3fleet linked thread")
		if err != nil {
			failTool("orchestration_error", "Could not authorize the link on its worker.")
			return
		}
		route.Peer = &peerRoute{Grant: key, WorkerToken: token}
		d.mu.Lock()
		err = d.saveDemandRouteLocked(id, route)
		d.mu.Unlock()
		if err != nil {
			failTool("orchestration_error", "Could not save the worker link.")
			return
		}
	}
	tool.Arguments["clientRequestId"] = slot
	res, err := d.sendPeerWorkerTool(r, worker, route.Peer.WorkerToken, request, tool)
	if err != nil {
		failTool("orchestration_error", "The linked worker did not answer; retry the same launch key.")
		return
	}
	if result := peerStructuredResult(res); result != nil {
		nativeID, _ := result["threadId"].(string)
		if !validDispatchThreadID(nativeID) || (id != slot && nativeID != id) || d.publishPeerThread(slot, id, nativeID) != nil {
			failTool("orchestration_error", "Could not publish the worker thread; retry the same launch key.")
			return
		}
	}
	d.writePeerResult(w, res, tool.Name)
}

func (d *dispatchPOC) publishPeerThread(slot, id, nativeID string) error {
	if id == nativeID {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.disk
	demand := *next.OnDemand
	route, ok := demand.Routes[id]
	if _, conflict := demand.Routes[nativeID]; !ok || conflict || route.Peer == nil {
		return errors.New("invalid peer route")
	}
	demand.Routes = maps.Clone(demand.Routes)
	delete(demand.Routes, id)
	demand.Routes[nativeID] = route
	demand.PeerThreads = maps.Clone(demand.PeerThreads)
	if demand.PeerThreads == nil {
		demand.PeerThreads = map[string]string{}
	}
	demand.PeerThreads[slot] = nativeID
	next.OnDemand = &demand
	if err := d.store.Save(next); err != nil {
		return err
	}
	d.disk = next
	return nil
}

func (d *dispatchPOC) callPeerThread(w http.ResponseWriter, r *http.Request, request peerRequest, tool peerTool, key string, grant peerGrant) {
	id, _ := tool.Arguments["threadId"].(string)
	d.mu.Lock()
	route := d.disk.OnDemand.Routes[id]
	d.mu.Unlock()
	if route.Peer == nil || route.Peer.Grant != key {
		peerToolError(w, request.ID, "capability_denied", "This thread does not belong to the link.")
		return
	}
	if grant.Access == "read-only" && tool.Name != "t3_thread_read" && tool.Name != "t3_thread_wait" {
		peerToolError(w, request.ID, "capability_denied", "This link is read-only.")
		return
	}
	worker, err := d.threadWorker(id)
	if err != nil {
		peerToolError(w, request.ID, "orchestration_error", "The linked worker is unavailable.")
		return
	}
	res, err := d.sendPeerWorkerTool(r, worker, route.Peer.WorkerToken, request, tool)
	if err != nil {
		peerToolError(w, request.ID, "orchestration_error", "The linked worker did not answer.")
		return
	}
	d.writePeerResult(w, res, tool.Name)
}

func (d *dispatchPOC) sendPeerWorkerTool(r *http.Request, worker DispatchWorker, token string, request peerRequest, tool peerTool) (map[string]any, error) {
	delete(tool.Arguments, "environmentId")
	request.Params, _ = json.Marshal(tool)
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(r.Context(), d.timeout)
	defer cancel()
	data, err := d.callWorkerPeer(ctx, worker, token, r.Header, body)
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if json.Unmarshal(peerResponseJSON(data), &reply) != nil || reply["jsonrpc"] != "2.0" {
		return nil, errors.New("invalid worker MCP result")
	}
	return reply, nil
}

func peerStructuredResult(reply map[string]any) map[string]any {
	result, _ := reply["result"].(map[string]any)
	if result["isError"] == true {
		return nil
	}
	data, _ := result["structuredContent"].(map[string]any)
	return data
}

func (d *dispatchPOC) writePeerToolResult(w http.ResponseWriter, body io.Reader, tool string) {
	data, err := io.ReadAll(io.LimitReader(body, maxJSON+1))
	var reply map[string]any
	if err != nil || len(data) > maxJSON || json.Unmarshal(peerResponseJSON(data), &reply) != nil || reply == nil {
		fail(w, 502, "invalid controller MCP result")
		return
	}
	d.writePeerResult(w, reply, tool)
}

func (d *dispatchPOC) peerModel() string {
	if d.onDemand.Model != "" {
		return d.onDemand.Model
	}
	return "gpt-5.4"
}

func (d *dispatchPOC) writePeerResult(w http.ResponseWriter, reply map[string]any, tool string) {
	if data := peerStructuredResult(reply); data != nil {
		d.mu.Lock()
		env := d.disk.EnvironmentID
		d.mu.Unlock()
		switch tool {
		case "t3_environment_read":
			data["environmentId"], data["label"] = env, dispatchLabel
		case "orchestrator_capabilities":
			data["providers"] = []any{map[string]any{
				"providerInstanceId":           "codex",
				"driverKind":                   "codex",
				"displayName":                  "Codex",
				"models":                       []any{map[string]string{"id": d.peerModel()}},
				"canRunChildTask":              false,
				"canRunCrossProviderChildTask": false,
				"constraints":                  []string{"t3fleet runs the configured model on demand; worker authentication is checked at launch."},
			}}
			if features, ok := data["features"].(map[string]any); ok {
				features["appOwnedSubagents"], features["batchThreadCreation"], features["scheduledTasks"] = false, false, false
				features["maxBatchThreads"] = 0
			}
		}
		thread := data
		if tool == "t3_thread_read" {
			thread, _ = data["thread"].(map[string]any)
		}
		if threadID, _ := thread["threadId"].(string); validDispatchThreadID(threadID) && thread["link"] != nil {
			thread["link"] = "<t3-thread://v2/" + env + "/" + threadID + ">"
		}
		// Keep the text fallback in sync and avoid disclosing the worker's native link.
		out, _ := reply["result"].(map[string]any)
		encoded, _ := json.Marshal(data)
		out["content"] = []any{map[string]string{"type": "text", "text": string(encoded)}}
	}
	writeJSON(w, reply)
}
