package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestLiveControllerPeerOAuth(t *testing.T) {
	controllerOrigin, service, _ := startLiveT3(t, func(base string) {
		t.Helper()
		userdata := filepath.Join(base, "userdata")
		if err := os.MkdirAll(userdata, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userdata, "settings.json"), []byte(`{"linkedEnvironmentsEnabled":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	origin := "http://" + server.Listener.Addr().String()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var controllerResolves atomic.Int64
	opts := DispatchOnDemand{
		Controller: DispatchWorker{ID: "controller", Resolve: func(context.Context) (Upstream, error) {
			controllerResolves.Add(1)
			return Upstream{Endpoint: provider.Endpoint{URL: controllerOrigin}, Token: service}, nil
		}},
		ProjectRoot: t.TempDir(),
		MaxWorkers:  1,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			t.Error("pairing provisioned a worker")
			return DispatchWorker{}, "", errors.New("no worker")
		},
		Lookup: func(string) (DispatchWorker, error) { return DispatchWorker{}, errors.New("no worker") },
	}
	p, err := NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	desktop, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.dispatch.saveToken(t.Context(), desktop); err != nil {
		t.Fatal(err)
	}
	localID := p.dispatch.disk.EnvironmentID
	opts.ExternalOrigin = &url.URL{Scheme: "https", Host: "fleet.example"}
	p, err = NewDispatchOnDemand(origin, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !p.authorized(&http.Request{Header: http.Header{"Authorization": {"Bearer " + desktop}}}) ||
		p.dispatch.disk.EnvironmentID != localID {
		t.Fatal("enabling the external origin lost the existing Desktop pairing or environment")
	}
	server.Config.Handler = p
	server.Start()
	t.Run("HTTPS reverse proxy preserves Desktop and peer access", func(t *testing.T) {
		testExternalDispatch(t, p, server.URL)
	})
	t.Run("Desktop can list outgoing links", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		client := newDispatchTestClient(t, ctx, server.URL, desktop)
		defer func() { _ = client.conn.CloseNow() }()
		links, ok := client.rpc("peerLinks.list", nil)["links"].([]any)
		if !ok || len(links) != 0 {
			t.Fatal("expected an empty outgoing link list from the fresh controller")
		}
	})
	t.Run("Desktop activity matches the native void reply", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		const method = "server.reportClientActivity"
		payload := map[string]any{
			"clientId": "test-desktop", "clientKind": "desktop-renderer", "visible": true,
			"focused": true, "recentlyInteracted": true, "scopes": []string{},
			"observedAt": time.Now().UTC().Format(time.RFC3339Nano),
		}
		var native map[string]any
		if err := p.dispatch.rpcTo(ctx, p.dispatch.onDemand.Controller, map[string]any{
			"_tag": "Request", "id": "native-activity", "tag": method, "payload": payload, "headers": []any{},
		}, nil, func(reply map[string]any) error {
			native, _ = reply["exit"].(map[string]any)
			return nil
		}); err != nil || native["_tag"] != "Success" {
			t.Fatalf("native activity report failed: %v, %v", native, err)
		}
		client := newDispatchTestClient(t, ctx, server.URL, desktop)
		defer func() { _ = client.conn.CloseNow() }()
		before := controllerResolves.Load()
		client.send("fleet-activity", method, payload)
		if got := client.next("fleet-activity")["exit"]; !reflect.DeepEqual(got, native) {
			t.Fatalf("fleet reply %v differs from native reply %v", got, native)
		}
		if controllerResolves.Load() != before {
			t.Fatal("fleet forwarded a client activity report to the controller")
		}
	})
	metadata, data := request(t, http.MethodGet, server.URL+"/.well-known/oauth-protected-resource/mcp", "")
	if metadata.StatusCode != http.StatusOK || !strings.Contains(string(data), server.URL+"/mcp") ||
		strings.Contains(string(data), controllerOrigin) {
		t.Fatal("OAuth discovery did not retain the virtual origin")
	}
	post := func(route, token, payload string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+route, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	pairPath := "/api/auth/pairing-token"
	peerPayload := `{"purpose":"peer-link","access":"approval-required"}`
	if status := post(pairPath, "", peerPayload); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pairing request returned %d", status)
	}
	if status := post(pairPath, desktop, `{"access":"full-access"}`); status != http.StatusBadRequest {
		t.Fatalf("desktop bearer could mint an unrelated credential: %d", status)
	}
	if status := post(pairPath, desktop, `{"Purpose":"peer-link","access":"full-access"}`); status != http.StatusBadRequest {
		t.Fatalf("a case alias was accepted in place of the native peer purpose: %d", status)
	}
	if status := post("/oauth/mcp/register", desktop, `{"software_id":"other","redirect_uris":["http://127.0.0.1/t3code-peer-link"]}`); status != http.StatusBadRequest {
		t.Fatalf("desktop bearer could register a generic OAuth client: %d", status)
	}
	if status := post("/mcp", desktop, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); status != http.StatusUnauthorized {
		t.Fatalf("desktop bearer could sign in to MCP: %d", status)
	}
	grant, err := p.dispatch.mintNativeWorkerPeer(t.Context(), DispatchWorker{Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: server.URL}, Token: desktop}, nil
	}}, "approval-required", "T3 Code · OAuth canary")
	if err != nil {
		t.Fatal(err)
	}
	native, ok := parsePeerNativeSession(grant)
	if !ok || native.Access != "approval-required" || !p.dispatch.hasPeerSession(native.SessionID) || !native.ExpiresAt.After(time.Now()) {
		t.Fatal("proxy did not save the native, scoped peer session")
	}
	if status := post(pairPath, grant, peerPayload); status != http.StatusUnauthorized {
		t.Fatalf("peer grant could mint another peer grant: %d", status)
	}
	public := DispatchWorker{Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: server.URL}, Token: desktop}, nil
	}}
	// The gateway checks Go's parsed method. The native controller must receive
	// the same method even when the input includes a conflicting case alias.
	result, err := p.dispatch.callWorkerPeer(t.Context(), public, grant, nil, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","Method":"ping","params":{"name":"unknown","arguments":{}}}`))
	if err != nil || checkNativePeerPing(bytes.NewReader(result)) != nil || !bytes.Contains(result, []byte(`"result":{}`)) {
		t.Fatalf("proxy and native controller did not agree on the MCP method: response %s, transport error %v", result, err)
	}
	revoke := "/api/auth/clients/revoke"
	marshalRevoke := func(id string) string {
		raw, _ := json.Marshal(map[string]string{"sessionId": id})
		return string(raw)
	}
	var serviceClaims struct {
		ID string `json:"sid"`
	}
	if !decodeNativeClaims(service, &serviceClaims) || serviceClaims.ID == "" {
		t.Fatal("could not identify the native service session")
	}
	if status := post(revoke, desktop, marshalRevoke(serviceClaims.ID)); status != http.StatusBadRequest {
		t.Fatalf("desktop could revoke the private controller service session: %d", status)
	}
	if status := post(revoke, desktop, marshalRevoke(native.SessionID)); status != http.StatusOK {
		t.Fatalf("could not revoke the native peer session: %d", status)
	}
	if status := post("/mcp", grant, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); status != http.StatusUnauthorized {
		t.Fatalf("revoked peer grant could still sign in: %d", status)
	}
}

func testExternalDispatch(t *testing.T, p *Proxy, local string) {
	t.Helper()
	target, err := url.Parse(local)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(front.Close)
	client := front.Client()
	tr := client.Transport.(*http.Transport)
	tr.TLSClientConfig.ServerName = "127.0.0.1" // Verify the loopback test certificate.
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, front.Listener.Addr().String())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	external := p.cfg.ExternalOrigin.String()
	pair, err := p.NewExternalPairing()
	if err != nil {
		t.Fatal(err)
	}
	pairURL, err := url.Parse(pair.URL)
	if err != nil || pairURL.Scheme != "https" || pairURL.Host != p.cfg.ExternalOrigin.Host {
		t.Fatal("external pairing did not advertise the HTTPS origin")
	}
	fragment, _ := url.ParseQuery(pairURL.Fragment)
	up := Upstream{Endpoint: provider.Endpoint{URL: external}}
	var exchange struct {
		Token string `json:"access_token"`
	}
	if err := workerPeerJSON(ctx, client, up, "", http.MethodPost, "/oauth/token",
		url.Values{"subject_token": {fragment.Get("token")}}, &exchange); err != nil || exchange.Token == "" {
		t.Fatalf("external Desktop exchange failed: %v", err)
	}
	ws := newDispatchTestClientHTTP(t, ctx, client, external, exchange.Token)
	defer func() { _ = ws.conn.CloseNow() }()
	cfg := ws.rpc("server.getConfig", nil)
	env, _ := cfg["environment"].(map[string]any)
	if env["environmentId"] != p.dispatch.disk.EnvironmentID {
		t.Fatal("external WebSocket advertised a different fleet identity")
	}
	var metadata struct{ Resource string }
	if err := workerPeerJSON(ctx, client, up, "", http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, &metadata); err != nil ||
		metadata.Resource != external+"/mcp" {
		t.Fatalf("external OAuth discovery did not use the configured origin: %v", err)
	}
	up.Token = exchange.Token
	grant, err := mintNativePeer(ctx, client, up, "read-only", "external OAuth test")
	if err != nil {
		t.Fatal(err)
	}
	req, err := workerPeerRequest(ctx, up, grant, http.MethodPost, "/mcp", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"external-test","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal("HTTPS peer MCP request failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("HTTPS peer MCP initialization did not establish a session: HTTP %d", res.StatusCode)
	}
}
