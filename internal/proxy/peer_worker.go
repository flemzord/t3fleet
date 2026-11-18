package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	workerPeerProtocol = "2025-06-18"
)

// The worker must see a genuine peer session. Sending a thread launch through
// the service account (or via the Desktop WebSocket) loses the link fence.
func (d *dispatchPOC) mintWorkerPeer(ctx context.Context, worker DispatchWorker, access, label string) (string, error) {
	frame := map[string]any{
		"_tag": "Request", "id": "peer-settings", "tag": "server.updateSettings", "headers": []any{},
		"payload": map[string]any{"patch": map[string]any{"linkedEnvironmentsEnabled": true}},
	}
	ok := false
	if err := d.rpcTo(ctx, worker, frame, nil, func(reply map[string]any) error {
		if exit, yes := reply["exit"].(map[string]any); yes && exit["_tag"] == "Success" {
			ok = true
		}
		return nil
	}); err != nil || !ok {
		return "", errors.New("could not enable the worker's native peer links")
	}
	return d.mintNativeWorkerPeer(ctx, worker, access, label)
}

func (d *dispatchPOC) mintNativeWorkerPeer(parent context.Context, worker DispatchWorker, access, label string) (string, error) {
	ctx, stop := context.WithTimeout(parent, d.timeout)
	defer stop()
	up, ctx, cancel, err := resolveUpstream(ctx, d.timeout, worker.Resolve)
	if err != nil {
		return "", err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: rejectRedirect}
	var metadata struct{ Resource string }
	if err := workerPeerJSON(ctx, client, up, "", http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, &metadata); err != nil {
		return "", fmt.Errorf("read worker MCP resource: %w", err)
	}
	resource, err := url.Parse(metadata.Resource)
	if err != nil || resource.Host == "" || resource.User != nil ||
		(resource.Scheme != "https" && resource.Scheme != "http") ||
		resource.Path != "/mcp" || resource.RawQuery != "" || resource.Fragment != "" {
		return "", errors.New("worker returned an invalid MCP resource")
	}
	var mint struct {
		Credential, Purpose, Access string
	}
	if err := workerPeerJSON(ctx, client, up, up.Token, http.MethodPost, "/api/auth/pairing-token",
		map[string]string{"purpose": "peer-link", "access": access, "label": label}, &mint); err != nil {
		return "", fmt.Errorf("mint worker peer code: %w", err)
	}
	if mint.Credential == "" || mint.Purpose != "peer-link" || mint.Access != access {
		return "", errors.New("worker did not mint the requested peer grant")
	}
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := workerPeerJSON(ctx, client, up, "", http.MethodPost, "/oauth/mcp/register",
		map[string]any{"client_name": label, "redirect_uris": []string{peerOAuthRedirect}, "software_id": peerOAuthSoftware}, &registration); err != nil {
		return "", fmt.Errorf("register worker peer: %w", err)
	}
	if registration.ClientID == "" {
		return "", errors.New("worker returned an invalid MCP client")
	}
	verifier, err := randomToken()
	if err != nil {
		return "", err
	}
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	var decision struct {
		RedirectTo string
	}
	if err := workerPeerJSON(ctx, client, up, "", http.MethodPost, "/oauth/mcp/decision", map[string]any{
		"authorization": map[string]string{"response_type": "code", "client_id": registration.ClientID,
			"redirect_uri": peerOAuthRedirect, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]),
			"code_challenge_method": "S256", "resource": metadata.Resource, "state": state},
		"decision": map[string]string{"_tag": "pairing-code", "access": access, "code": mint.Credential},
	}, &decision); err != nil {
		return "", fmt.Errorf("approve worker peer: %w", err)
	}
	redirect, err := url.Parse(decision.RedirectTo)
	if err != nil || redirect.Query().Get("state") != state || redirect.Query().Get("code") == "" {
		return "", errors.New("worker rejected the peer grant")
	}
	code := redirect.Query().Get("code")
	redirect.RawQuery, redirect.Fragment = "", ""
	if redirect.String() != peerOAuthRedirect {
		return "", errors.New("worker returned an invalid peer redirect")
	}
	var grant struct {
		Token     string `json:"access_token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := workerPeerJSON(ctx, client, up, "", http.MethodPost, "/oauth/mcp/token", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {registration.ClientID}, "redirect_uri": {peerOAuthRedirect},
		"resource": {metadata.Resource}, "code": {code}, "code_verifier": {verifier},
	}, &grant); err != nil {
		return "", fmt.Errorf("exchange worker peer code: %w", err)
	}
	claims, ok := parsePeerNativeSession(grant.Token)
	if grant.ExpiresIn <= 0 || !ok || claims.Access != access || !time.Now().Before(claims.ExpiresAt) || !validWorkerPeerCredential(grant.Token) {
		return "", errors.New("worker returned an invalid peer credential")
	}
	return grant.Token, nil
}

func validWorkerPeerCredential(value string) bool {
	return value != "" && len(value) <= 8192 && !strings.ContainsAny(value, " \t\r\n")
}

func workerPeerRequest(ctx context.Context, up Upstream, token, method, path string, body []byte) (*http.Request, error) {
	target := targetURL(up.Endpoint, &http.Request{URL: &url.URL{Path: path}}, false)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid worker peer request")
	}
	req.Header = upstreamHeaders(up.Endpoint, token)
	if token == "" {
		req.Header.Del("Authorization")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func workerPeerJSON(ctx context.Context, client *http.Client, up Upstream, token, method, path string, body, result any) error {
	var data []byte
	var form bool
	switch body := body.(type) {
	case nil:
	case url.Values:
		form = true
		data = []byte(body.Encode())
	default:
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("invalid worker OAuth request")
		}
	}
	req, err := workerPeerRequest(ctx, up, token, method, path, data)
	if err != nil {
		return err
	}
	if form {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("worker OAuth request failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("worker replied with status %d", res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON || json.Unmarshal(data, result) != nil {
		return errors.New("invalid worker OAuth response")
	}
	return nil
}

// Keep the worker's authorization and caller limits together. The session ID
// belongs to the native worker MCP transport; never forward the controller ID.
func (d *dispatchPOC) callWorkerPeer(parent context.Context, worker DispatchWorker, token string, headers http.Header, body []byte) ([]byte, error) {
	if !validWorkerPeerCredential(token) {
		return nil, errors.New("missing worker peer credential")
	}
	ctx, stop := context.WithTimeout(parent, d.timeout)
	defer stop()
	up, ctx, cancel, err := resolveUpstream(ctx, d.timeout, worker.Resolve)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	tr.ResponseHeaderTimeout = 0 // A native tool may wait; the request context still has a deadline.
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: rejectRedirect}
	exchange := func(ctx context.Context, method, session string, data []byte) ([]byte, string, error) {
		req, err := workerPeerRequest(ctx, up, token, method, "/mcp", data)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		if limit := headers.Get("T3-Mode-Limit"); limit != "" {
			req.Header.Set("T3-Mode-Limit", limit)
		}
		if sender := headers.Get("T3-Sender-Thread"); sender != "" {
			req.Header.Set("T3-Sender-Thread", sender)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", workerPeerProtocol)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, "", errors.New("worker MCP request failed")
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, "", fmt.Errorf("worker MCP replied with status %d", res.StatusCode)
		}
		if method == http.MethodDelete {
			return nil, "", nil
		}
		answer, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
		if err != nil || len(answer) > maxJSON {
			return nil, "", errors.New("invalid worker MCP response")
		}
		return workerPeerResponseJSON(answer, res.Header.Get("Content-Type")), res.Header.Get("Mcp-Session-Id"), nil
	}

	initialize := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t3fleet-peer","version":"1"}}}`)
	_, session, err := exchange(ctx, http.MethodPost, "", initialize)
	if err != nil {
		return nil, err
	}
	if !validWorkerPeerCredential(session) {
		return nil, errors.New("worker did not create an MCP session")
	}
	defer func() {
		// Finish the short-lived transport session without extending the caller's deadline.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, _, _ = exchange(cleanup, http.MethodDelete, session, nil)
	}()
	if _, _, err := exchange(ctx, http.MethodPost, session, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return nil, err
	}
	answer, _, err := exchange(ctx, http.MethodPost, session, body)
	if err != nil {
		return nil, err
	}
	var request, response struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 ||
		json.Unmarshal(answer, &response) != nil || !bytes.Equal(request.ID, response.ID) {
		return nil, errors.New("worker did not return the expected JSON-RPC response")
	}
	return answer, nil
}

func workerPeerResponseJSON(body []byte, contentType string) []byte {
	if !strings.Contains(contentType, "text/event-stream") {
		return bytes.TrimSpace(body)
	}
	var event []byte
	var response []byte
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case len(line) == 0:
			var message struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(event, &message) == nil && len(message.ID) > 0 {
				response = bytes.Clone(bytes.TrimSpace(event))
			}
			event = event[:0]
		case bytes.HasPrefix(line, []byte("data:")):
			event = append(event, bytes.TrimPrefix(line[len("data:"):], []byte(" "))...)
			event = append(event, '\n')
		}
	}
	return response
}
