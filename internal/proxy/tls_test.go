package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// A process may have used the default transport for HTTP/2 provider API calls
// before it constructs a T3 connection. Clone must not leave stale HTTP/2 ALPN
// configuration behind when the gateway requires HTTP/1.1 for WebSockets.
func TestTLSHTTP2PrivateEndpointPairing(t *testing.T) {
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "boat-grant=private-cookie" {
			t.Error("private endpoint cookie was not preserved")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Error("T3 service bearer was not preserved")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/prime":
			writeJSON(w, map[string]any{"protocol": r.Proto})
		case "/.well-known/t3/environment":
			writeJSON(w, map[string]any{"environmentId": "tls-environment", "label": "remote", "orchestrationProtocolVersion": 2})
		case "/api/auth/session":
			writeJSON(w, map[string]any{"scopes": []string{"orchestration:read"}})
		case "/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			_ = conn.Write(r.Context(), kind, data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	source.EnableHTTP2 = true
	source.StartTLS()
	t.Cleanup(source.Close)
	primed := source.Client().Transport.(*http.Transport).Clone()
	primed.ForceAttemptHTTP2 = true
	old := http.DefaultTransport
	http.DefaultTransport = primed
	t.Cleanup(func() { http.DefaultTransport = old; primed.CloseIdleConnections() })
	prime, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, source.URL+"/prime", nil)
	prime.Header.Set("Cookie", "boat-grant=private-cookie")
	prime.Header.Set("Authorization", "Bearer service-secret")
	reply, err := (&http.Client{Transport: primed}).Do(prime)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ProtoMajor != 2 {
		t.Fatalf("test failed to prime HTTP/2: %s", reply.Proto)
	}
	_ = reply.Body.Close()

	p, server := proxyServer(t, source.URL, func(cfg *Config) {
		cfg.Resolve = func(context.Context) (Upstream, error) {
			return Upstream{Endpoint: provider.Endpoint{URL: source.URL, Headers: http.Header{"Cookie": {"boat-grant=private-cookie"}}}, Token: "service-secret"}, nil
		}
	})
	token := exchange(t, p, server)
	res, _ := request(t, http.MethodGet, server.URL+"/api/auth/session", token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("private TLS auth-session status=%d", res.StatusCode)
	}
	ticket := ticketValue(t, server, token, "rpc")
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http")+"/ws?wsTicket="+url.QueryEscape(ticket), nil)
	if err != nil {
		t.Fatal("private TLS websocket did not connect")
	}
	defer conn.CloseNow()
	if err := conn.Write(t.Context(), websocket.MessageText, []byte("private-frame")); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.Read(t.Context())
	if err != nil || string(body) != "private-frame" {
		t.Fatalf("private TLS websocket relay failed: %v", err)
	}
}
