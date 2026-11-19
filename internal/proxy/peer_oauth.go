package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	peerOAuthSoftware = "t3code-peer-link"
	peerOAuthRedirect = "http://127.0.0.1/t3code-peer-link"
	maxPeerOAuthBody  = 32 << 10
)

type peerNativeSession struct {
	SessionID string
	Access    string
	ExpiresAt time.Time
}

// The pinned T3 server signs a base64url JSON payload followed by a signature.
// Decode claims only to describe a grant issued by a trusted native T3 server.
// A decoded client credential is never proof of its validity or revocation state.
func decodeNativeClaims(value string, dst any) bool {
	payload, signature, ok := strings.Cut(value, ".")
	if !ok || payload == "" || signature == "" || strings.Contains(signature, ".") || len(value) > maxPeerOAuthBody {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(raw, dst) == nil
}

func parsePeerNativeSession(token string) (peerNativeSession, bool) {
	var claims struct {
		Version int      `json:"v"`
		Kind    string   `json:"kind"`
		ID      string   `json:"sid"`
		Subject string   `json:"sub"`
		Method  string   `json:"method"`
		Peer    int      `json:"plk"`
		Ceiling string   `json:"rtc"`
		Scopes  []string `json:"scopes"`
		Expiry  int64    `json:"exp"`
	}
	if !decodeNativeClaims(token, &claims) || claims.Version != 1 || claims.Kind != "session" ||
		claims.ID == "" || claims.Subject != "mcp-client" || claims.Method != "bearer-access-token" ||
		claims.Peer != 1 || claims.Expiry < 1 || !slices.Contains(claims.Scopes, "orchestration:read") {
		return peerNativeSession{}, false
	}
	access := "read-only"
	if slices.Contains(claims.Scopes, "orchestration:operate") {
		if claims.Ceiling == "" || claims.Ceiling == "read-only" || !validPeerOAuthAccess(claims.Ceiling) {
			return peerNativeSession{}, false
		}
		access = claims.Ceiling
	} else if claims.Ceiling != "" {
		return peerNativeSession{}, false
	}
	return peerNativeSession{SessionID: claims.ID, Access: access, ExpiresAt: time.UnixMilli(claims.Expiry)}, true
}

func validPeerOAuthAccess(access string) bool {
	switch access {
	case "read-only", "approval-required", "auto-accept-edits", "auto", "full-access":
		return true
	default:
		return false
	}
}

func nativePeerOAuthClient(id string) bool {
	var claims struct {
		Version int      `json:"v"`
		Peer    int      `json:"p"`
		URIs    []string `json:"r"`
	}
	// This guard restricts the relayed protocol; native OAuth verifies the signature.
	return decodeNativeClaims(id, &claims) && claims.Version == 1 && claims.Peer == 1 &&
		len(claims.URIs) == 1 && claims.URIs[0] == peerOAuthRedirect
}

// NewPeerPairing returns a one-use code from the native controller. The fleet
// admin API authenticates its caller before invoking it.
func (p *Proxy) NewPeerPairing(ctx context.Context, access string) (string, error) {
	if p.dispatch == nil || !validPeerOAuthAccess(access) {
		return "", errors.New("peer pairing requires a fleet endpoint and a valid access level")
	}
	body, _ := json.Marshal(map[string]string{"purpose": "peer-link", "access": access})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.origin.String()+"/api/auth/pairing-token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	status, payload, err := requestPeerOAuth(p, req, body, true)
	var reply struct{ Credential, Purpose, Access string }
	if err != nil || status != http.StatusOK || json.Unmarshal(payload, &reply) != nil ||
		reply.Purpose != "peer-link" || reply.Access != access || !validWorkerPeerCredential(reply.Credential) {
		return "", errors.New("controller could not issue a peer pairing code")
	}
	return reply.Credential, nil
}

// servePeerOAuth sends just the native linked-environment sign-in routes to the
// dedicated controller. Desktop pairing and peer authorization remain separate.
func (d *dispatcher) servePeerOAuth(p *Proxy, w http.ResponseWriter, r *http.Request) bool {
	admin := false
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server":
		if r.Method != http.MethodGet {
			return false
		}
	case "/oauth/mcp/register", "/oauth/mcp/decision", "/oauth/mcp/token":
		if r.Method != http.MethodPost {
			return false
		}
	case "/api/auth/pairing-token", "/api/auth/pairing-links/revoke", "/api/auth/clients/revoke":
		if r.Method != http.MethodPost {
			return false
		}
		admin = true
	default:
		return false
	}

	if admin && !p.authorized(r) {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return true
	}
	if !admin && r.Header.Get("Origin") != "" {
		fail(w, http.StatusForbidden, "forbidden origin")
		return true
	}
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "unexpected peer OAuth query")
		return true
	}
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "peer OAuth routes do not accept query parameters")
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPeerOAuthBody+1))
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid peer OAuth request")
		return true
	}
	if len(body) > maxPeerOAuthBody {
		fail(w, http.StatusRequestEntityTooLarge, "peer OAuth request too large")
		return true
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !d.acceptPeerOAuth(r.URL.Path, contentType, body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		switch r.URL.Path {
		case "/oauth/mcp/register":
			writeJSON(w, map[string]string{"error": "invalid_client_metadata", "error_description": "Only T3 linked environment registration is supported."})
		case "/oauth/mcp/decision":
			writeJSON(w, map[string]string{"_tag": "AuthMcpApprovalError", "message": "Only a linked environment pairing code is supported."})
		case "/oauth/mcp/token":
			writeJSON(w, map[string]string{"error": "invalid_request", "error_description": "Only linked environment authorization code exchange is supported."})
		default:
			writeJSON(w, map[string]string{"error": "invalid linked environment administration request"})
		}
		return true
	}

	if contentType == "application/json" {
		// Native T3 reads JSON keys case sensitively. Re-encode the exact
		// keys that were validated to remove any duplicate representations.
		var value map[string]any
		if json.Unmarshal(body, &value) != nil || value == nil {
			fail(w, http.StatusBadRequest, "invalid peer OAuth JSON")
			return true
		}
		body, _ = json.Marshal(value)
	}
	status, payload, err := requestPeerOAuth(p, r, body, admin)
	if err != nil {
		fail(w, http.StatusBadGateway, "linked environment authentication is unavailable")
		return true
	}
	if r.URL.Path == "/oauth/mcp/token" && status == http.StatusOK {
		var reply struct {
			Token     string `json:"access_token"`
			TokenType string `json:"token_type"`
			ExpiresIn int64  `json:"expires_in"`
		}
		if json.Unmarshal(payload, &reply) != nil || reply.TokenType != "Bearer" || reply.ExpiresIn < 1 {
			fail(w, http.StatusBadGateway, "controller returned an invalid linked environment grant")
			return true
		}
		now := time.Now()
		grant, ok := parsePeerNativeSession(reply.Token)
		if !ok || !grant.ExpiresAt.After(now) {
			fail(w, http.StatusBadGateway, "controller returned an invalid linked environment grant")
			return true
		}
		if err := d.savePeerGrant(reply.Token, grant.Access, grant.ExpiresAt); err != nil {
			fail(w, http.StatusServiceUnavailable, "could not save the linked environment grant")
			return true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	return true
}

func (d *dispatcher) acceptPeerOAuth(path, contentType string, body []byte) bool {
	if path == "/oauth/mcp/token" {
		form, err := url.ParseQuery(string(body))
		if contentType != "application/x-www-form-urlencoded" || err != nil {
			return false
		}
		for _, values := range form {
			if len(values) != 1 {
				return false
			}
		}
		return form.Get("grant_type") == "authorization_code" && nativePeerOAuthClient(form.Get("client_id")) &&
			form.Get("redirect_uri") == peerOAuthRedirect
	}
	if strings.HasPrefix(path, "/.well-known/") {
		return len(body) == 0
	}
	var request map[string]any
	if contentType != "application/json" || json.Unmarshal(body, &request) != nil || request == nil {
		return false
	}
	field := func(obj map[string]any, key string) string {
		value, _ := obj[key].(string)
		return value
	}
	switch path {
	case "/oauth/mcp/register":
		uris, _ := request["redirect_uris"].([]any)
		return field(request, "software_id") == peerOAuthSoftware && len(uris) == 1 && uris[0] == peerOAuthRedirect
	case "/oauth/mcp/decision":
		auth, _ := request["authorization"].(map[string]any)
		decision, _ := request["decision"].(map[string]any)
		return nativePeerOAuthClient(field(auth, "client_id")) && field(auth, "redirect_uri") == peerOAuthRedirect &&
			field(decision, "_tag") == "pairing-code" && validPeerOAuthAccess(field(decision, "access")) && field(decision, "code") != ""
	case "/api/auth/pairing-token":
		return field(request, "purpose") == "peer-link" && validPeerOAuthAccess(field(request, "access"))
	case "/api/auth/clients/revoke":
		return d.hasPeerSession(field(request, "sessionId"))
	case "/api/auth/pairing-links/revoke":
		return field(request, "id") != ""
	default:
		return false
	}
}

func requestPeerOAuth(p *Proxy, r *http.Request, body []byte, admin bool) (int, []byte, error) {
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	up, ctx, cancel, err := p.resolve(r.WithContext(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	transport := transport(up.Endpoint)
	defer transport.CloseIdleConnections()
	// Build a fresh request: public OAuth never sees the controller's service
	// token, Desktop or caller cookies, or caller-provided forwarding headers.
	target := targetURL(up.Endpoint, r, false)
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	// Native OAuth verifies the resource and issuer against this origin; keep
	// the already validated public Host even though the controller port moves.
	req.Host = r.Host
	if p.origin.Scheme == "https" {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if admin {
		req.Header.Set("Authorization", "Bearer "+up.Token)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxPeerOAuthBody+1))
	if err != nil || len(data) > maxPeerOAuthBody || (res.StatusCode >= 300 && res.StatusCode < 400) {
		return 0, nil, errors.New("invalid native OAuth response")
	}
	return res.StatusCode, data, nil
}
