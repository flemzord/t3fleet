package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// TestLiveT3Compatibility is opt-in: it launches a genuine T3 server with a
// disposable home, without using a provider account, opening a browser, or
// touching the developer's T3 database. No credentials enter test diagnostics.
func startLiveT3(t *testing.T, setup func(baseDir string)) (string, string, map[string]any) {
	t.Helper()
	binary := os.Getenv("T3FLEET_T3_BINARY")
	if binary == "" {
		t.Skip("set T3FLEET_T3_BINARY to test a genuine T3 server")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("T3FLEET_T3_BINARY must be an absolute executable path")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal("T3 test binary is unavailable")
	}

	home := t.TempDir()
	baseDir := filepath.Join(home, "t3-data")
	if setup != nil {
		setup(baseDir)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	origin := "http://" + address
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("Git is required for the genuine T3 fixture")
	}
	// Avoid discovering a real Codex/Claude executable from the developer
	// shell. Otherwise T3 starts its background jobs in the disposable home.
	path := strings.Join([]string{filepath.Dir(git), "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, string(os.PathListSeparator))
	env := append(os.Environ(), "PATH="+path, "HOME="+home, "T3CODE_HOME="+baseDir, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "XDG_DATA_HOME="+filepath.Join(home, "data"))
	ctx, cancel := context.WithCancel(t.Context())
	command := exec.CommandContext(ctx, binary, "serve", "--mode", "desktop", "--host", "127.0.0.1", "--port", port, "--base-dir", baseDir, "--no-browser")
	command.Dir = home
	command.Env = env
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(os.Interrupt)
	}
	command.WaitDelay = 5 * time.Second
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal("could not start disposable T3 server")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("disposable T3 process did not exit")
			}
		}
	})

	client := &http.Client{Timeout: time.Second}
	ready, readyCancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer readyCancel()
	var descriptor map[string]any
	for {
		req, _ := http.NewRequestWithContext(ready, http.MethodGet, origin+"/.well-known/t3/environment", nil)
		response, requestErr := client.Do(req)
		if requestErr == nil {
			if response.StatusCode == http.StatusOK {
				_ = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&descriptor)
			}
			_ = response.Body.Close()
			if descriptor["environmentId"] != nil {
				break
			}
		}
		select {
		case <-ready.Done():
			t.Fatal("disposable T3 server did not become ready")
		case <-time.After(100 * time.Millisecond):
		}
		select {
		case <-done:
			t.Fatal("disposable T3 server exited before readiness")
		default:
		}
	}

	issueCtx, issueCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer issueCancel()
	issue := exec.CommandContext(issueCtx, binary, "auth", "session", "issue", "--base-dir", baseDir, "--token-only", "--ttl", "1h", "--label", "t3fleet-test")
	issue.Dir = home
	issue.Env = env
	issue.Stderr = io.Discard
	issued, err := issue.Output()
	if err != nil {
		t.Fatal("could not issue disposable T3 service session")
	}
	serviceToken := strings.TrimSpace(string(issued))
	if serviceToken == "" || strings.ContainsAny(serviceToken, "\r\n") {
		t.Fatal("disposable T3 service session did not return a token")
	}
	return origin, serviceToken, descriptor
}

func TestLiveT3Compatibility(t *testing.T) {
	origin, serviceToken, descriptor := startLiveT3(t, nil)
	gateway, server := proxyServer(t, origin, func(cfg *Config) {
		cfg.Label = "live-compatibility"
		cfg.Resolve = func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: serviceToken}, nil
		}
	})
	response, body := request(t, "GET", server.URL+"/.well-known/t3/environment", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live descriptor status=%d", response.StatusCode)
	}
	var throughProxy map[string]any
	if err := json.Unmarshal(body, &throughProxy); err != nil {
		t.Fatal("proxy returned invalid descriptor JSON")
	}
	if throughProxy["environmentId"] != descriptor["environmentId"] || throughProxy["label"] != "live-compatibility" {
		t.Fatal("proxy changed the remote identity or failed to set the label")
	}
	if throughProxy["orchestrationProtocolVersion"] != float64(2) {
		t.Fatalf("live T3 uses an unsupported orchestration protocol: %v", throughProxy["orchestrationProtocolVersion"])
	}

	pair, err := gateway.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	pairURL, err := url.Parse(pair.URL)
	if err != nil {
		t.Fatal("invalid pairing URL")
	}
	fragment, _ := url.ParseQuery(pairURL.Fragment)
	pairResponse, err := http.PostForm(server.URL+"/oauth/token", url.Values{"subject_token": {fragment.Get("token")}})
	if err != nil {
		t.Fatal("live pairing exchange request failed")
	}
	defer func() { _ = pairResponse.Body.Close() }()
	if pairResponse.StatusCode != http.StatusOK {
		t.Fatalf("live pairing exchange status=%d", pairResponse.StatusCode)
	}
	var exchange map[string]any
	if err := json.NewDecoder(pairResponse.Body).Decode(&exchange); err != nil {
		t.Fatal("live pairing exchange returned invalid JSON")
	}
	token, _ := exchange["access_token"].(string)
	if token == "" {
		t.Fatal("live pairing exchange did not issue a client token")
	}
	response, body = request(t, "GET", server.URL+"/api/auth/session", token)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live proxied auth-session status=%d", response.StatusCode)
	}
	var session map[string]any
	if json.Unmarshal(body, &session) != nil || session["scopes"] == nil {
		t.Fatal("live proxied session has no scope information")
	}

	rpcTicket := ticketValue(t, server, token, "rpc")
	socketCtx, socketCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer socketCancel()
	socket, _, err := websocket.Dial(socketCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws?wsTicket="+url.QueryEscape(rpcTicket), nil)
	if err != nil {
		t.Fatal("could not connect to genuine T3 through gateway WebSocket")
	}
	defer socket.CloseNow()
	socket.SetReadLimit(maxWSMessage)
	message := []byte(`{"_tag":"Request","id":"live-config","tag":"server.getConfig","payload":{},"headers":[]}`)
	if err := socket.Write(socketCtx, websocket.MessageText, message); err != nil {
		t.Fatal("could not send genuine T3 config RPC")
	}
	configReceived := false
	for !configReceived {
		kind, data, err := socket.Read(socketCtx)
		if err != nil {
			t.Fatalf("genuine T3 config RPC did not return a response: %s", strings.ReplaceAll(fmt.Sprint(err), serviceToken, "<REDACTED>"))
		}
		if kind != websocket.MessageText {
			continue
		}
		frames, _, ok := messages(data)
		if !ok {
			t.Fatal("genuine T3 returned an unsupported RPC frame encoding")
		}
		for _, frame := range frames {
			if frame["_tag"] != "Exit" || frame["requestId"] != "live-config" {
				continue
			}
			exit, _ := frame["exit"].(map[string]any)
			if exit["_tag"] != "Success" {
				t.Fatal("genuine T3 rejected server.getConfig")
			}
			config, ok := exit["value"].(map[string]any)
			if !ok {
				t.Fatal("genuine T3 config RPC returned unexpected payload")
			}
			if _, exists := config["directEndpoints"]; exists {
				t.Fatal("genuine T3 directEndpoints escaped proxy sanitization")
			}
			configReceived = true
		}
	}

	shellRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/orchestration/shell?omitIdleSubagents=1&omitOlderSettled=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	shellRequest.Header.Set("Authorization", "Bearer "+token)
	shellRequest.Header.Set("X-T3-Orchestration-Protocol", "2")
	response, err = http.DefaultClient.Do(shellRequest)
	if err != nil {
		t.Fatal("genuine T3 orchestration shell request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		t.Fatal("could not read genuine T3 orchestration shell")
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("genuine T3 orchestration shell status=%d", response.StatusCode)
	}
	var shell any
	if json.Unmarshal(body, &shell) != nil {
		t.Fatal("genuine T3 orchestration shell is not JSON")
	}
	// Fail if the upstream credential accidentally appears in public HTTP output.
	if strings.Contains(string(body), serviceToken) {
		t.Fatal("genuine T3 service token leaked into shell response")
	}
	t.Logf("genuine T3 version %v: pairing, HTTP session, WebSocket config and orchestration shell verified", descriptor["serverVersion"])
}
