package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
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

func (d *dispatcher) savePeerGrant(token, access string, expiresAt time.Time) error {
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
	if err := d.saveDispatchState(next); err != nil {
		return errors.New("could not persist the peer grant")
	}
	d.disk = next
	return nil
}

func (d *dispatcher) hasPeerSession(sid string) bool {
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

func (d *dispatcher) controllerMCPRequest(r *http.Request, body []byte, origin *url.URL) (*http.Response, func(), error) {
	up, ctx, cancel, err := resolveUpstream(r.Context(), d.timeout, d.onDemand.Controller.Resolve)
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
	setPeerAuthority(req, r, origin)
	res, err := tr.RoundTrip(req)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return res, func() { _ = res.Body.Close(); cleanup() }, nil
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

func (d *dispatcher) servePeerMCP(w http.ResponseWriter, r *http.Request, origin *url.URL) {
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
			res, closeRes, err := d.controllerMCPRequest(r, body, origin)
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
	check, closeCheck, err := d.controllerMCPRequest(r, ping, origin)
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
	case "t3_environment_read", "t3_project_list", "t3_project_read", "orchestrator_capabilities":
		request.Params, _ = json.Marshal(tool)
		body, _ := json.Marshal(request)
		res, closeRes, err := d.controllerMCPRequest(r, body, origin)
		if err != nil {
			fail(w, 502, "peer controller unavailable")
			return
		}
		defer closeRes()
		if res.StatusCode != http.StatusOK {
			relayPeerMCPResponse(w, res)
			return
		}
		d.writePeerToolResult(w, res.Body, tool.Name)
	case "t3_project_clone", "t3_project_create":
		if grant.Access != "full-access" || r.Header.Get("T3-Mode-Limit") != "full-access/default" {
			peerToolError(w, request.ID, "capability_denied", "Registering a repository needs a full-access link and a full-access/default caller.")
			return
		}
		result, err := d.peerProject(r.Context(), tool)
		if err != nil {
			peerToolError(w, request.ID, "invalid_request", err.Error())
			return
		}
		d.writePeerResult(w, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"structuredContent": result}}, tool.Name)
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

func (d *dispatcher) launchPeerThread(w http.ResponseWriter, r *http.Request, request peerRequest, tool peerTool, key string, grant peerGrant) {
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
	if requested, _ := tool.Arguments["modelSelection"].(map[string]any); requested != nil {
		instance, _ := requested["instanceId"].(string)
		if requested["instanceId"] == nil {
			instance, _ = requested["provider"].(string)
		}
		model, _ := requested["model"].(string)
		if !d.onDemand.offers(instance, model) {
			failTool("invalid_request", "This t3fleet environment only runs the agents and models listed by orchestrator_capabilities.")
			return
		}
	} else {
		agent := d.onDemand.agents()[0]
		tool.Arguments["modelSelection"] = map[string]string{"instanceId": agent.Driver, "model": agent.Models[0]}
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
	d.mu.Unlock()
	unlock := d.lockDemandWrite(id)
	defer unlock()
	route := d.demandRoute(id)
	if route.ProjectID != "" && (route.ProjectID != projectID || (route.Peer != nil && route.Peer.Grant != key)) {
		failTool("invalid_request", "A launch retry must use the original project and link.")
		return
	}
	worker, err := d.prepareDemandWorker(r.Context(), id, projectID)
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
	if route.PoolSlot != 0 {
		tool.Arguments["workspaceStrategy"] = fixedWorkspaceStrategy(strategy)
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

func (d *dispatcher) publishPeerThread(slot, id, nativeID string) error {
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
	if err := d.saveDispatchState(next); err != nil {
		return err
	}
	d.disk = next
	return nil
}

func (d *dispatcher) callPeerThread(w http.ResponseWriter, r *http.Request, request peerRequest, tool peerTool, key string, grant peerGrant) {
	id, _ := tool.Arguments["threadId"].(string)
	if tool.Name != "t3_thread_read" && tool.Name != "t3_thread_wait" {
		unlock := d.lockDemandWrite(id)
		defer unlock()
	}
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
	if tool.Name == "t3_thread_organize" && tool.Arguments["action"] == "archive" && d.handlesDemandArchive(id) {
		d.archiveDemandPeer(w, r, request, tool, id, route)
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

func (d *dispatcher) sendPeerWorkerTool(r *http.Request, worker DispatchWorker, token string, request peerRequest, tool peerTool) (map[string]any, error) {
	defer worker.begin()()
	delete(tool.Arguments, "environmentId")
	request.Params, _ = json.Marshal(tool)
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(r.Context(), d.timeout)
	defer cancel()
	up, ctx, stop, err := resolveUpstream(ctx, d.timeout, worker.Resolve)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer closeEndpoint(up.Endpoint)
	data, err := callWorkerPeerAt(ctx, up, token, r.Header, body)
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if json.Unmarshal(peerResponseJSON(data), &reply) != nil || reply["jsonrpc"] != "2.0" {
		return nil, errors.New("invalid worker MCP result")
	}
	if result := peerStructuredResult(reply); result != nil {
		id, _ := tool.Arguments["threadId"].(string)
		routeID := id
		if tool.Name == "t3_thread_launch" {
			id, _ = result["threadId"].(string)
			slot, _ := tool.Arguments["clientRequestId"].(string)
			d.mu.Lock()
			routeID = d.disk.OnDemand.PeerThreads[slot]
			d.mu.Unlock()
			if routeID == "" {
				routeID = slot
			}
		}
		if action := tool.Arguments["action"]; tool.Name == "t3_thread_organize" && (action == "archive" || action == "unarchive") {
			if err := d.noteFixedArchive(id, action == "archive"); err != nil {
				return nil, err
			}
		}
		if err := d.observeThreadListing(ctx, up, routeID, id, tool.Name == "t3_thread_launch"); err != nil {
			return nil, err
		}
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

func (d *dispatcher) writePeerToolResult(w http.ResponseWriter, body io.Reader, tool string) {
	data, err := io.ReadAll(io.LimitReader(body, maxJSON+1))
	var reply map[string]any
	if err != nil || len(data) > maxJSON || json.Unmarshal(peerResponseJSON(data), &reply) != nil || reply == nil {
		fail(w, 502, "invalid controller MCP result")
		return
	}
	d.writePeerResult(w, reply, tool)
}

func (d *dispatcher) writePeerResult(w http.ResponseWriter, reply map[string]any, tool string) {
	if data := peerStructuredResult(reply); data != nil {
		d.mu.Lock()
		env := d.disk.EnvironmentID
		d.mu.Unlock()
		switch tool {
		case "t3_project_list":
			projects, _ := data["projects"].([]any)
			for _, item := range projects {
				project, _ := item.(map[string]any)
				d.decorateProject(project)
			}
		case "t3_project_read", "t3_project_create":
			d.decorateProject(data)
		case "t3_environment_read":
			data["environmentId"], data["label"] = env, dispatchLabel
		case "orchestrator_capabilities":
			var providers []any
			for _, agent := range d.onDemand.agents() {
				models := make([]any, 0, len(agent.Models))
				for _, model := range agent.Models {
					models = append(models, map[string]string{"id": model})
				}
				providers = append(providers, map[string]any{
					"providerInstanceId":           agent.Driver,
					"driverKind":                   agent.Driver,
					"displayName":                  agentDisplayName(agent.Driver),
					"models":                       models,
					"canRunChildTask":              false,
					"canRunCrossProviderChildTask": false,
					"constraints":                  []string{"t3fleet runs the configured models on demand; worker installation and authentication are checked at launch."},
				})
			}
			data["providers"] = providers
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
