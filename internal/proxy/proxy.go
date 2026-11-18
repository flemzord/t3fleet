// Package proxy presents a stable, authenticated T3 endpoint for one machine.
// The protocol behavior is based on the MIT-licensed t3boat implementation by
// Pau Kraft (2026); this implementation is written independently in Go.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

const (
	maxBody        = 128 << 20
	maxJSON        = 4 << 20
	maxWSMessage   = 16 << 20
	clientLifetime = 365 * 24 * time.Hour
)

// Upstream is a live T3 server connection. Neither Endpoint nor Token is public.
type Upstream struct {
	Endpoint provider.Endpoint
	Token    string
}

// Config supplies machine lifecycle and persisted client authorization.
// Resolve wakes the machine and waits for its T3 server to become ready. It is
// called only after request authorization, except for the public descriptor.
// Supply both Authorize and SaveToken to keep pairings across daemon restarts.
// A successful SaveToken must make Authorize accept that token immediately.
// Persisted tokens must expire one year after issuance, matching the T3 lease.
type Config struct {
	Label          string
	Origin         string
	Resolve        func(context.Context) (Upstream, error)
	Authorize      func(string) bool
	SaveToken      func(context.Context, string) error
	RequestTimeout time.Duration
}

type Pairing struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type credential struct {
	value   string
	expires time.Time
}
type ticket struct {
	use     string
	expires time.Time
}

// Proxy owns ephemeral pairing codes and scoped socket tickets. Client token
// persistence belongs to the fleet state store, rather than the HTTP module.
type Proxy struct {
	cfg     Config
	origin  *url.URL
	mu      sync.Mutex
	pairing credential
	tickets map[string]ticket
	clients map[string]time.Time
}

func New(cfg Config) (*Proxy, error) {
	origin, err := url.Parse(cfg.Origin)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("proxy origin must be an absolute HTTP(S) origin")
	}
	if cfg.Resolve == nil {
		return nil, errors.New("proxy resolver is required")
	}
	if (cfg.Authorize == nil) != (cfg.SaveToken == nil) {
		return nil, errors.New("proxy authorization and persistence callbacks must be configured together")
	}
	if cfg.RequestTimeout < 0 {
		return nil, errors.New("proxy request timeout must be positive")
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}
	origin.Path = ""
	return &Proxy{cfg: cfg, origin: origin, tickets: map[string]ticket{}, clients: map[string]time.Time{}}, nil
}

// Handler returns the machine's public T3 protocol handler.
func (p *Proxy) Handler() http.Handler { return p }

// NewPairing replaces the previous unredeemed code. The URL is a credential and
// must be displayed only to the user who requested pairing, never logged.
func (p *Proxy) NewPairing() (Pairing, error) {
	code, err := randomToken()
	if err != nil {
		return Pairing{}, err
	}
	expires := time.Now().Add(15 * time.Minute)
	p.mu.Lock()
	p.pairing = credential{code, expires}
	p.mu.Unlock()
	return Pairing{URL: p.origin.String() + "/pair#token=" + code, ExpiresAt: expires}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !p.validHost(r.Host) || !p.validOrigin(r.Header.Get("Origin")) {
		fail(w, http.StatusForbidden, "forbidden origin")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, DPoP, X-T3-Orchestration-Protocol, B3, Traceparent")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.ContentLength > maxBody {
		fail(w, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	switch {
	case r.URL.Path == "/.well-known/t3/environment" && r.Method == http.MethodGet:
		p.descriptor(w, r)
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		p.exchange(w, r)
	case r.URL.Path == "/api/auth/session" && r.Method == http.MethodGet:
		if !p.authorized(r) {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		p.session(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/auth/websocket-ticket/") && r.Method == http.MethodPost:
		if !p.authorized(r) {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		p.issueTicket(w, r)
	default:
		use := ""
		if r.URL.Path == "/ws" {
			use = "rpc"
		} else if mediaPath(r.URL.Path) || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			use = "media"
		}
		authorized := p.authorized(r)
		if !authorized && use != "" && r.URL.Query().Get("wsTicket") != "" {
			authorized = p.redeemTicket(r.URL.Query().Get("wsTicket"), use)
		}
		if !authorized {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			p.socket(w, r)
			return
		}
		p.relay(w, r)
	}
}

func mediaPath(path string) bool {
	return strings.HasPrefix(path, "/api/device-hub/") || strings.HasPrefix(path, "/api/preview-stream/")
}

func (p *Proxy) validHost(host string) bool {
	got, err := url.Parse("http://" + host)
	if err != nil || got.Host != host || got.User != nil {
		return false
	}
	a, b := strings.ToLower(got.Hostname()), strings.ToLower(p.origin.Hostname())
	if got.Port() != p.origin.Port() {
		return false
	}
	return a == b || (loopback(a) && loopback(b))
}
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (p *Proxy) validOrigin(origin string) bool {
	return origin == "" || origin == "t3code://app" || origin == p.origin.String()
}
func (p *Proxy) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	if p.cfg.Authorize != nil {
		return p.cfg.Authorize(token)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.clients[token])
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("could not generate credential")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (p *Proxy) consumeCode(code string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if code == "" || time.Now().After(p.pairing.expires) || subtle.ConstantTimeCompare([]byte(code), []byte(p.pairing.value)) != 1 {
		return false
	}
	p.pairing = credential{}
	return true
}
func (p *Proxy) issueTicket(w http.ResponseWriter, r *http.Request) {
	use := strings.TrimPrefix(r.URL.Path, "/api/auth/websocket-ticket/")
	if use != "rpc" && use != "media" {
		fail(w, http.StatusNotFound, "unknown ticket scope")
		return
	}
	value, err := randomToken()
	if err != nil {
		fail(w, http.StatusInternalServerError, "credential generation failed")
		return
	}
	expires := time.Now().Add(5 * time.Minute)
	p.mu.Lock()
	for key, t := range p.tickets {
		if !time.Now().Before(t.expires) {
			delete(p.tickets, key)
		}
	}
	if len(p.tickets) >= 4096 {
		p.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "too many active tickets")
		return
	}
	p.tickets[value] = ticket{use, expires}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"ticket": value, "expiresAt": expires.UTC().Format(time.RFC3339Nano)})
}
func (p *Proxy) redeemTicket(value, use string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.tickets[value]
	if !ok || t.use != use || !time.Now().Before(t.expires) {
		return false
	}
	if use == "rpc" {
		delete(p.tickets, value)
	}
	return true
}

// resolve bounds startup without imposing a fixed lifetime on established
// streaming responses or WebSockets. Cancellation still follows the client.
func (p *Proxy) resolve(r *http.Request) (Upstream, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(r.Context())
	timer := time.AfterFunc(p.cfg.RequestTimeout, cancel)
	up, err := p.cfg.Resolve(ctx)
	timer.Stop()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		u, parseErr := url.Parse(up.Endpoint.URL)
		if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || up.Token == "" || strings.ContainsAny(up.Token, "\r\n") {
			err = errors.New("invalid upstream")
		}
	}
	if err != nil {
		closeEndpoint(up.Endpoint)
		cancel()
		return Upstream{}, nil, nil, errors.New("machine is not ready")
	}
	return up, ctx, cancel, nil
}
func closeEndpoint(ep provider.Endpoint) {
	if ep.Close != nil {
		_ = ep.Close()
	}
}
func transport(ep provider.Endpoint) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = 30 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	if ep.DialContext != nil {
		t.DialContext = ep.DialContext
	}
	t.ForceAttemptHTTP2 = false
	return t
}
func targetURL(ep provider.Endpoint, r *http.Request, ws bool) *url.URL {
	target, _ := url.Parse(ep.URL)
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawPath = ""
	q := r.URL.Query()
	q.Del("wsTicket")
	if ws && r.URL.Path == "/ws" {
		q.Del("frameParts")
		q.Set("orchestrationProtocol", "2")
	}
	base := target.Query()
	for k, values := range q {
		if _, reserved := base[k]; !reserved {
			base[k] = values
		}
	}
	target.RawQuery = base.Encode()
	if ws {
		if target.Scheme == "https" {
			target.Scheme = "wss"
		} else {
			target.Scheme = "ws"
		}
	}
	return target
}
func upstreamHeaders(ep provider.Endpoint, token string) http.Header {
	headers := ep.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+token)
	return headers
}
func (p *Proxy) requestJSON(r *http.Request, path string) (map[string]any, error) {
	up, ctx, cancel, err := p.resolve(r)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	relative := r.Clone(ctx)
	relative.URL = &url.URL{Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL(up.Endpoint, relative, false).String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = upstreamHeaders(up.Endpoint, up.Token)
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("upstream unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return nil, errors.New("invalid upstream response")
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil || result == nil {
		return nil, errors.New("invalid upstream response")
	}
	return result, nil
}
func (p *Proxy) descriptor(w http.ResponseWriter, r *http.Request) {
	descriptor, err := p.requestJSON(r, "/.well-known/t3/environment")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "machine is not ready")
		return
	}
	descriptor["label"] = p.cfg.Label
	delete(descriptor, "directEndpoints")
	writeJSON(w, descriptor)
}
func (p *Proxy) session(w http.ResponseWriter, r *http.Request) {
	session, err := p.requestJSON(r, "/api/auth/session")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "machine is not ready")
		return
	}
	session["expiresAt"] = time.Now().Add(clientLifetime).UTC().Format(time.RFC3339Nano)
	writeJSON(w, session)
}
func (p *Proxy) exchange(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, "invalid token exchange")
		return
	}
	if !p.consumeCode(r.PostForm.Get("subject_token")) {
		fail(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	session, err := p.requestJSON(r, "/api/auth/session")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "machine is not ready; generate a new pairing link")
		return
	}
	token, err := randomToken()
	if err != nil {
		fail(w, http.StatusInternalServerError, "credential generation failed")
		return
	}
	if p.cfg.SaveToken != nil {
		if err := p.cfg.SaveToken(r.Context(), token); err != nil {
			fail(w, http.StatusInternalServerError, "could not persist pairing")
			return
		}
	} else {
		p.mu.Lock()
		p.clients[token] = time.Now().Add(clientLifetime)
		p.mu.Unlock()
	}
	body := map[string]any{"access_token": token, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "token_type": "Bearer", "expires_in": int(clientLifetime.Seconds())}
	if scopes, ok := session["scopes"].([]any); ok {
		values := make([]string, 0, len(scopes))
		for _, s := range scopes {
			if value, ok := s.(string); ok {
				values = append(values, value)
			}
		}
		body["scope"] = strings.Join(values, " ")
	}
	if permissions, ok := session["permissions"]; ok {
		body["permissions"] = permissions
	}
	writeJSON(w, body)
}
func (p *Proxy) relay(w http.ResponseWriter, r *http.Request) {
	up, ctx, cancel, err := p.resolve(r)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "machine is not ready")
		return
	}
	defer cancel()
	defer closeEndpoint(up.Endpoint)
	tr := transport(up.Endpoint)
	defer tr.CloseIdleConnections()
	target := targetURL(up.Endpoint, r, false)
	relay := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL = target
			out.Out.Host = target.Host
			out.Out.Header.Del("Authorization")
			out.Out.Header.Del("Cookie")
			out.Out.Header.Del("Proxy-Authorization")
			for k := range out.Out.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-forwarded-") {
					out.Out.Header.Del(k)
				}
			}
			for k, values := range upstreamHeaders(up.Endpoint, up.Token) {
				out.Out.Header[k] = values
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			fail(w, http.StatusBadGateway, "upstream request failed")
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if value := res.Header.Get("Location"); value != "" {
				location, err := url.Parse(value)
				if err != nil {
					return errors.New("invalid upstream redirect")
				}
				if location.Host == "" || location.Host == target.Host {
					query := location.Query()
					endpointURL, _ := url.Parse(up.Endpoint.URL)
					for key := range endpointURL.Query() {
						query.Del(key)
					}
					location.RawQuery = query.Encode()
					if location.Host != "" {
						location.Scheme = p.origin.Scheme
						location.Host = p.origin.Host
						location.Path = strings.TrimPrefix(location.Path, strings.TrimRight(endpointURL.Path, "/"))
					}
					res.Header.Set("Location", location.String())
				}
			}
			return nil
		},
	}
	relay.ServeHTTP(w, r.WithContext(ctx))
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// String avoids accidentally exposing endpoint credentials when a proxy is
// included in a diagnostic message.
func (p *Proxy) String() string { return fmt.Sprintf("T3 proxy (%s)", p.cfg.Label) }
