package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	p, err := NewDispatchOnDemand(origin, store, DispatchOnDemand{
		Controller: DispatchWorker{ID: "controller", Resolve: func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: controllerOrigin}, Token: service}, nil
		}},
		ProjectRoot: t.TempDir(),
		MaxWorkers:  1,
		Provision: func(context.Context, string, string) (DispatchWorker, string, error) {
			t.Error("pairing provisioned a worker")
			return DispatchWorker{}, "", errors.New("no worker")
		},
		Lookup: func(string) (DispatchWorker, error) { return DispatchWorker{}, errors.New("no worker") },
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = p
	server.Start()
	desktop, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.dispatch.saveToken(t.Context(), desktop); err != nil {
		t.Fatal(err)
	}
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
		defer response.Body.Close()
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
