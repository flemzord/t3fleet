package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func proxyServer(t *testing.T, upstream string, customize func(*Config)) (*Proxy, *httptest.Server) {
	t.Helper()
	s := httptest.NewUnstartedServer(nil)
	cfg := Config{Label: "workbench", Origin: "http://" + s.Listener.Addr().String(), Resolve: func(context.Context) (Upstream, error) {
		return Upstream{Endpoint: provider.Endpoint{URL: upstream}, Token: "service-secret"}, nil
	}}
	if customize != nil {
		customize(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Handler = p
	s.Start()
	t.Cleanup(s.Close)
	return p, s
}
func sourceServer(t *testing.T, extra http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/.well-known/t3/environment":
			writeJSON(w, map[string]any{"environmentId": "remote-id", "label": "remote", "orchestrationProtocolVersion": 2, "directEndpoints": []string{"http://bypass"}})
		case "/api/auth/session":
			writeJSON(w, map[string]any{"scopes": []string{"orchestration:read", "orchestration:write"}, "permissions": map[string]any{"filesystem": "read-write"}, "expiresAt": "2020-01-01T00:00:00Z"})
		default:
			if extra != nil {
				extra(w, r)
			} else {
				w.WriteHeader(204)
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}
func exchange(t *testing.T, p *Proxy, s *httptest.Server) string {
	t.Helper()
	pair, err := p.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(pair.URL)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.PostForm(s.URL+"/oauth/token", url.Values{"subject_token": {fragment.Get("token")}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("pair status %d: %s", res.StatusCode, b)
	}
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	token, _ := result["access_token"].(string)
	if token == "" {
		t.Fatal("missing client token")
	}
	return token
}
func request(t *testing.T, method, target, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}
func ticketValue(t *testing.T, s *httptest.Server, token, use string) string {
	t.Helper()
	res, b := request(t, "POST", s.URL+"/api/auth/websocket-ticket/"+use, token)
	if res.StatusCode != 200 {
		t.Fatalf("ticket %d: %s", res.StatusCode, b)
	}
	var data map[string]string
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	return data["ticket"]
}

func TestDescriptorPairingAndAuthenticatedHTTP(t *testing.T) {
	var closes atomic.Int32
	upstream := sourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("caller cookie reached machine")
		}
		if r.Header.Get("X-Forwarded-Host") != "" {
			t.Error("forwarded host reached machine")
		}
		if r.URL.Query().Get("_token") != "provider-secret" {
			t.Error("provider credential lost")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-T3-Test", "yes")
		_, _ = w.Write(append([]byte("received:"), body...))
	})
	p, s := proxyServer(t, upstream.URL+"?_token=provider-secret", func(c *Config) {
		c.Resolve = func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: upstream.URL + "?_token=provider-secret", Close: func() error { closes.Add(1); return nil }}, Token: "service-secret"}, nil
		}
	})
	res, b := request(t, "GET", s.URL+"/.well-known/t3/environment", "")
	if res.StatusCode != 200 {
		t.Fatalf("descriptor: %d %s", res.StatusCode, b)
	}
	var descriptor map[string]any
	_ = json.Unmarshal(b, &descriptor)
	if descriptor["label"] != "workbench" || descriptor["environmentId"] != "remote-id" {
		t.Fatalf("descriptor %s", b)
	}
	if _, ok := descriptor["directEndpoints"]; ok {
		t.Fatal("bypass endpoint remained")
	}
	token := exchange(t, p, s)
	req, _ := http.NewRequestWithContext(t.Context(), "POST", s.URL+"/api/file?key=value&_token=attacker", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "private-cookie")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(data) != "received:payload" || response.Header.Get("X-T3-Test") != "yes" {
		t.Fatalf("relay status=%d body=%s", response.StatusCode, data)
	}
	res, b = request(t, "GET", s.URL+"/api/auth/session", token)
	if res.StatusCode != 200 || strings.Contains(string(b), "2020-01-01") {
		t.Fatalf("session %d %s", res.StatusCode, b)
	}
	if closes.Load() != 4 {
		t.Fatalf("closed endpoints=%d, want 4", closes.Load())
	}
}

func TestRequestSecurityDoesNotWakeUnauthorizedMachine(t *testing.T) {
	var resolves atomic.Int32
	p, s := proxyServer(t, "http://127.0.0.1:1", func(c *Config) {
		c.Resolve = func(context.Context) (Upstream, error) {
			resolves.Add(1)
			return Upstream{}, errors.New("https://provider/?_token=secret")
		}
	})
	for _, tc := range []struct {
		name, method, path, host, origin, auth string
		want                                   int
	}{
		{name: "no token", method: "GET", path: "/api/file", want: 401},
		{name: "unknown token", method: "GET", path: "/api/file", auth: "Bearer wrong", want: 401},
		{name: "DNS rebinding", method: "GET", path: "/.well-known/t3/environment", host: "evil.example", want: 403},
		{name: "browser CSRF", method: "GET", path: "/.well-known/t3/environment", origin: "https://evil.example", want: 403},
		{name: "bad code", method: "POST", path: "/oauth/token", want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), tc.method, s.URL+tc.path, nil)
			if tc.host != "" {
				req.Host = tc.host
			}
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Authorization", tc.auth)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status %d want %d", res.StatusCode, tc.want)
			}
		})
	}
	if resolves.Load() != 0 {
		t.Fatalf("unauthorized calls woke machine %d times", resolves.Load())
	}
	pair, err := p.NewPairing()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pair.URL, s.URL+"/pair#token=") || time.Until(pair.ExpiresAt) < 14*time.Minute {
		t.Fatalf("bad pairing %+v", pair)
	}
	res, b := request(t, "GET", s.URL+"/.well-known/t3/environment", "")
	if res.StatusCode != 503 || strings.Contains(string(b), "secret") {
		t.Fatalf("error leaked upstream: %d %s", res.StatusCode, b)
	}
}

func TestPairingCodesAreSingleUseAndReplaceable(t *testing.T) {
	upstream := sourceServer(t, nil)
	p, s := proxyServer(t, upstream.URL, nil)
	pair, _ := p.NewPairing()
	first := strings.TrimPrefix(pair.URL, s.URL+"/pair#token=")
	second, _ := p.NewPairing()
	code := strings.TrimPrefix(second.URL, s.URL+"/pair#token=")
	post := func(code string) int {
		res, err := http.PostForm(s.URL+"/oauth/token", url.Values{"subject_token": {code}})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if post(first) != 400 {
		t.Fatal("replaced pairing code was accepted")
	}
	if post(code) != 200 || post(code) != 400 {
		t.Fatal("pairing code was replayable")
	}
	pair, _ = p.NewPairing()
	code = strings.TrimPrefix(pair.URL, s.URL+"/pair#token=")
	p.mu.Lock()
	p.pairing.expires = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if post(code) != 400 {
		t.Fatal("expired pairing code was accepted")
	}
}

func TestPersistentPairingCallbacks(t *testing.T) {
	upstream := sourceServer(t, nil)
	var mu sync.Mutex
	tokens := map[string]bool{}
	p, s := proxyServer(t, upstream.URL, func(c *Config) {
		c.Authorize = func(token string) bool { mu.Lock(); defer mu.Unlock(); return tokens[token] }
		c.SaveToken = func(_ context.Context, token string) error {
			mu.Lock()
			defer mu.Unlock()
			tokens[token] = true
			return nil
		}
	})
	token := exchange(t, p, s)
	res, _ := request(t, "GET", s.URL+"/api/auth/session", token)
	if res.StatusCode != 200 {
		t.Fatalf("saved token refused: %d", res.StatusCode)
	}
	mu.Lock()
	delete(tokens, token)
	mu.Unlock()
	res, _ = request(t, "GET", s.URL+"/api/auth/session", token)
	if res.StatusCode != 401 {
		t.Fatalf("revoked token accepted: %d", res.StatusCode)
	}
}

func TestTicketScopesReplayAndExpiration(t *testing.T) {
	upstream := sourceServer(t, nil)
	p, s := proxyServer(t, upstream.URL, nil)
	token := exchange(t, p, s)
	rpc := ticketValue(t, s, token, "rpc")
	if p.redeemTicket(rpc, "media") {
		t.Fatal("RPC ticket accepted for media")
	}
	if !p.redeemTicket(rpc, "rpc") || p.redeemTicket(rpc, "rpc") {
		t.Fatal("RPC ticket is not single use")
	}
	media := ticketValue(t, s, token, "media")
	if !p.redeemTicket(media, "media") || !p.redeemTicket(media, "media") {
		t.Fatal("media ticket must support multiple stream requests")
	}
	res, _ := request(t, "GET", s.URL+"/api/orchestration/shell?wsTicket="+media, "")
	if res.StatusCode != 401 {
		t.Fatalf("media ticket reached admin-style route: %d", res.StatusCode)
	}
	res, _ = request(t, "GET", s.URL+"/api/device-hub/test?wsTicket="+media, "")
	if res.StatusCode != 204 {
		t.Fatalf("media HTTP ticket refused: %d", res.StatusCode)
	}
	p.mu.Lock()
	p.tickets[media] = ticket{"media", time.Now().Add(-time.Second)}
	p.mu.Unlock()
	if p.redeemTicket(media, "media") {
		t.Fatal("expired media ticket accepted")
	}
	res, _ = request(t, "POST", s.URL+"/api/auth/websocket-ticket/unknown", token)
	if res.StatusCode != 404 {
		t.Fatalf("unknown ticket use: %d", res.StatusCode)
	}
}

func TestCanceledWakeAndEndpointCleanup(t *testing.T) {
	var canceled atomic.Bool
	var closed atomic.Bool
	_, s := proxyServer(t, "http://127.0.0.1:1", func(c *Config) {
		c.RequestTimeout = 10 * time.Millisecond
		c.Resolve = func(ctx context.Context) (Upstream, error) {
			<-ctx.Done()
			canceled.Store(true)
			return Upstream{Endpoint: provider.Endpoint{Close: func() error { closed.Store(true); return nil }}}, ctx.Err()
		}
	})
	res, _ := request(t, "GET", s.URL+"/.well-known/t3/environment", "")
	if res.StatusCode != 503 || !canceled.Load() || !closed.Load() {
		t.Fatalf("wake not canceled/closed: %d %v %v", res.StatusCode, canceled.Load(), closed.Load())
	}
}

func TestWebSocketRelayAuthenticatesAndSanitizesServerConfig(t *testing.T) {
	upstream := sourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("wsTicket") != "" || r.URL.Query().Get("orchestrationProtocol") != "2" || r.URL.Query().Get("frameParts") != "" {
			t.Errorf("upstream query %s", r.URL.RawQuery)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			kind, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				_ = conn.Write(r.Context(), kind, b)
				continue
			}
			var req map[string]any
			if json.Unmarshal(b, &req) != nil {
				return
			}
			response := map[string]any{"_tag": "Exit", "requestId": req["id"], "exit": map[string]any{"_tag": "Success", "value": map[string]any{"directEndpoints": []string{"https://bypass"}, "normal": "preserved"}}}
			data, _ := json.Marshal(response)
			if conn.Write(r.Context(), websocket.MessageText, data) != nil {
				return
			}
		}
	})
	p, s := proxyServer(t, upstream.URL, nil)
	token := exchange(t, p, s)
	ticket := ticketValue(t, s, token, "rpc")
	target := "ws" + strings.TrimPrefix(s.URL, "http") + "/ws?wsTicket=" + ticket + "&frameParts=1"
	conn, _, err := websocket.Dial(t.Context(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"_tag":"Request","id":"1","tag":"server.getConfig","payload":{}}`)); err != nil {
		t.Fatal(err)
	}
	_, b, err := conn.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "directEndpoints") || !strings.Contains(string(b), "preserved") {
		t.Fatalf("unsanitized config: %s", b)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"_tag":"Request","id":"2","tag":"some.userData","payload":{}}`)); err != nil {
		t.Fatal(err)
	}
	_, b, err = conn.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "directEndpoints") {
		t.Fatalf("unrelated user data changed: %s", b)
	}
	if err := conn.Write(t.Context(), websocket.MessageBinary, []byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	kind, b, err := conn.Read(t.Context())
	if err != nil || kind != websocket.MessageBinary || string(b) != string([]byte{0, 1, 2}) {
		t.Fatalf("binary relay: %v %v %v", kind, b, err)
	}
	_, response, err := websocket.Dial(t.Context(), target, nil)
	if err == nil || response.StatusCode != 401 {
		t.Fatalf("RPC ticket replay accepted: %v", err)
	}
}

func TestRPCConfigSubscriptionSanitization(t *testing.T) {
	r := rpcRequests{tags: map[string]string{}}
	r.observe([]byte(`[{"_tag":"Request","id":12,"tag":"subscribeServerConfig"},{"_tag":"Request","id":13,"tag":"server.getConfig"}]`))
	result := r.sanitize([]byte(`[{"_tag":"Chunk","requestId":12,"values":[{"config":{"directEndpoints":["secret"],"keep":true}}]},{"_tag":"Exit","requestId":13,"exit":{"_tag":"Success","value":{"directEndpoints":[],"keep":true}}}]`))
	if strings.Contains(string(result), "directEndpoints") || strings.Count(string(result), "keep") != 2 {
		t.Fatalf("bad subscription rewrite: %s", result)
	}
	if _, ok := r.tags["13"]; ok {
		t.Fatal("completed RPC remained in tracking map")
	}
	unchanged := []byte("invalid json")
	if string(r.sanitize(unchanged)) != string(unchanged) {
		t.Fatal("non-RPC data changed")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, origin := range []string{"", "://bad", "ftp://localhost:1", "http://user:pass@localhost:1", "http://localhost:1/path", "http://localhost:1?token=secret"} {
		t.Run(origin, func(t *testing.T) {
			if _, err := New(Config{Origin: origin, Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }}); err == nil {
				t.Fatal("invalid origin accepted")
			}
		})
	}
	if _, err := New(Config{Origin: "http://localhost:1"}); err == nil {
		t.Fatal("missing resolver accepted")
	}
	if _, err := New(Config{Origin: "http://localhost:1", Resolve: func(context.Context) (Upstream, error) { return Upstream{}, nil }, Authorize: func(string) bool { return true }}); err == nil {
		t.Fatal("half-configured persistence accepted")
	}
}

func TestUpstreamFailuresAreRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = fmt.Fprint(w, "private-token")
	}))
	defer upstream.Close()
	_, s := proxyServer(t, upstream.URL+"?_token=private-token", nil)
	res, b := request(t, "GET", s.URL+"/.well-known/t3/environment", "")
	if res.StatusCode != 503 || strings.Contains(string(b), "private-token") {
		t.Fatalf("credential leak: %d %s", res.StatusCode, b)
	}
}

func TestRedirectDoesNotExposeProviderEndpointCredential(t *testing.T) {
	var source string
	upstream := sourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", source+"/next?_token=provider-secret&keep=1")
		w.Header().Set("Set-Cookie", "upstream-cookie=secret")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	source = upstream.URL
	p, s := proxyServer(t, upstream.URL+"?_token=provider-secret", nil)
	token := exchange(t, p, s)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", s.URL+"/redirect", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Location") != s.URL+"/next?keep=1" || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("redirect leaks private endpoint: %v", res.Header)
	}
}
