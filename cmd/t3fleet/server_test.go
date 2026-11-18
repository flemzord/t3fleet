package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type fakeFleet struct {
	calls   []string
	err     error
	created fleet.CreateRequest
}

func (f *fakeFleet) Create(_ context.Context, r fleet.CreateRequest) (fleet.Machine, error) {
	f.calls = append(f.calls, "create")
	f.created = r
	return fleet.Machine{ID: "machine-1"}, f.err
}
func (f *fakeFleet) PrepareBatch(_ context.Context, r fleet.BatchRequest) (fleet.Batch, error) {
	return fleet.Batch{ID: r.ID}, f.err
}
func (f *fakeFleet) Batch(id string) (fleet.Batch, error) { return fleet.Batch{ID: id}, f.err }
func (f *fakeFleet) List() []fleet.Machine                { return []fleet.Machine{{ID: "machine-1"}} }
func (f *fakeFleet) Get(id string) (fleet.Machine, error) { return fleet.Machine{ID: id}, f.err }
func (f *fakeFleet) Start(_ context.Context, id string) error {
	f.calls = append(f.calls, "start:"+id)
	return f.err
}
func (f *fakeFleet) Stop(_ context.Context, id string) error {
	f.calls = append(f.calls, "stop:"+id)
	return f.err
}
func (f *fakeFleet) Delete(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete:"+id)
	return f.err
}
func (f *fakeFleet) Snapshot(_ context.Context, id, name string) (provider.Snapshot, error) {
	f.calls = append(f.calls, "snapshot:"+id+":"+name)
	return provider.Snapshot{ID: name, Ready: true}, f.err
}
func (f *fakeFleet) Pair(_ context.Context, id string) (proxy.Pairing, error) {
	f.calls = append(f.calls, "pair:"+id)
	return proxy.Pairing{URL: "http://127.0.0.1:47800/pair#token=one-time-code", ExpiresAt: time.Now().Add(time.Minute)}, f.err
}

func adminRequest(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:47730"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminAuthorizationAndHostIsolation(t *testing.T) {
	f := &fakeFleet{}
	h := newAdminHandler(f, "private-admin-token")
	for _, path := range []string{"/v1/machines", "/v1/machines/x/start", "/v1/machines/x/pair"} {
		w := adminRequest(h, http.MethodPost, path, "", "{}")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted without auth: %d", path, w.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("unauthorized request reached fleet")
	}
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Host = "attacker.example:47730" },
		func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
	} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:47730/v1/machines", nil)
		r.Header.Set("Authorization", "Bearer private-admin-token")
		change(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("unsafe request accepted: %d", w.Code)
		}
	}
	for _, path := range []string{"/_healthcheck", "/_info"} {
		w := adminRequest(h, http.MethodGet, path, "", "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "private-admin-token") {
			t.Fatal("unsafe health/info response")
		}
	}
}

func TestAdminLifecycleRoutesAndValidation(t *testing.T) {
	f := &fakeFleet{}
	h := newAdminHandler(f, "token")
	cases := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/machines", "", 200},
		{"POST", "/v1/machines", `{"name":"demo","provider":"boat"}`, 201},
		{"GET", "/v1/machines/machine-1", "", 200},
		{"POST", "/v1/machines/machine-1/start", "", 204},
		{"POST", "/v1/machines/machine-1/stop", "", 204},
		{"POST", "/v1/machines/machine-1/pair", "", 200},
		{"POST", "/v1/machines/machine-1/snapshot", `{"name":"golden"}`, 200},
		{"DELETE", "/v1/machines/machine-1", "", 204},
		{"GET", "/v1/machines/machine-1/start", "", 405},
		{"POST", "/v1/machines/machine-1/nope", "", 404},
		{"GET", "/missing", "", 404},
		{"POST", "/v1/machines", `{"name":"demo","provider":"boat","secret":"bad"}`, 400},
		{"POST", "/v1/machines", `{"name":"demo"}`, 400},
		{"POST", "/v1/machines", `{} {}`, 400},
		{"POST", "/v1/machines/machine-1/snapshot", `{}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			w := adminRequest(h, tc.method, tc.path, "token", tc.body)
			if w.Code != tc.status {
				t.Fatalf("got %d (%s), want %d", w.Code, w.Body.String(), tc.status)
			}
		})
	}
	if f.created.Name != "demo" || f.created.Provider != "boat" {
		t.Fatal("request did not reach manager")
	}
	if got := strings.Join(f.calls, ","); got != "create,start:machine-1,stop:machine-1,pair:machine-1,snapshot:machine-1:golden,delete:machine-1" {
		t.Fatalf("calls: %s", got)
	}
}

func TestAdminErrorsNeverExposeProviderSecrets(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{errors.New("private-url?_token=secret"), 502, "operation_failed"},
		{provider.ErrNotFound, 404, "not_found"},
		{provider.ErrUnsupported, 501, "unsupported"},
		{fleet.ErrRuntimeUnavailable, 422, "runtime_unavailable"},
		{context.DeadlineExceeded, 504, "operation_failed"},
	} {
		h := newAdminHandler(&fakeFleet{err: tc.err}, "token")
		w := adminRequest(h, "POST", "/v1/machines/x/start", "token", "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("unsafe response: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestPrivateAdminTokenPersistence(t *testing.T) {
	dir := t.TempDir()
	token, err := ensureAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatal("invalid token length")
	}
	again, err := ensureAdminToken(dir)
	if err != nil || again != token {
		t.Fatal("token changed")
	}
	path := filepath.Join(dir, "admin-token")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("token must be private")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAdminToken(dir); err == nil {
		t.Fatal("unsafe token permissions accepted")
	}
	linkdir := t.TempDir()
	if err := os.Symlink(path, filepath.Join(linkdir, "admin-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAdminToken(linkdir); err == nil {
		t.Fatal("symlink token accepted")
	}
}

func TestCLIThroughAuthenticatedAdminAPI(t *testing.T) {
	fake := &fakeFleet{}
	token, err := ensureAdminToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "admin-token"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newAdminHandler(fake, token))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	commands := [][]string{
		{"create", "--name", "demo", "--provider", "boat", "--snapshot", "golden", "--no-register"},
		{"list"}, {"inspect", "machine-1"}, {"start", "machine-1"}, {"stop", "machine-1"}, {"snapshot", "machine-1", "--name", "copy"}, {"pair", "machine-1"}, {"delete", "machine-1"},
	}
	for _, args := range commands {
		var out bytes.Buffer
		if err := runClient(t.Context(), cfg, args[0], args[1:], &out, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if args[0] == "pair" && !strings.HasPrefix(out.String(), "t3code://pair?pairingUrl=") {
			t.Fatalf("invalid pairing deep link: %s", out.String())
		}
	}
	if fake.created.Template.Snapshot != "golden" {
		t.Fatal("template override lost")
	}
}

func TestClientCancellationAndErrorRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"credential-secret"}`)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	_, err := callAdmin(t.Context(), address, "admin-token", "GET", "/v1/machines", nil)
	if err == nil || strings.Contains(err.Error(), "credential-secret") {
		t.Fatal("error leaked vendor body")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = callAdmin(ctx, address, "admin-token", "GET", "/v1/machines", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestClientExplainsNameConflictForCurrentAndOlderDaemons(t *testing.T) {
	for _, body := range []string{
		`{"error":"conflict","machine":{"id":"2c9951085a1fbe87f586f8f370706f6b","state":"create-failed"}}`,
		`{"error":"conflict"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, body)
		}))
		_, err := callAdmin(t.Context(), strings.TrimPrefix(server.URL, "http://"), "token", "POST", "/v1/machines", map[string]string{"name": "mon-test2"})
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "name already exists") || !strings.Contains(err.Error(), "--name") {
			t.Fatalf("conflict did not explain name reservation: %v", err)
		}
		if strings.Contains(body, "2c9951085a1fbe87f586f8f370706f6b") && !strings.Contains(err.Error(), "2c9951085a1fbe87f586f8f370706f6b") {
			t.Fatal("conflict did not identify the existing record")
		}
	}
}

func TestClientExplainsMissingRuntimeWithoutLosingAllocatedIdentity(t *testing.T) {
	const id = "f2e1a7e3f84914948dfe3f04869a79ef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"runtime_unavailable","machine":{"id":"`+id+`"},"details":"private-command-output"}`)
	}))
	defer server.Close()
	_, err := callAdmin(t.Context(), strings.TrimPrefix(server.URL, "http://"), "token", "POST", "/v1/machines", nil)
	if err == nil || !strings.Contains(err.Error(), "t3fleet-runtime-info") || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "remains allocated") || strings.Contains(err.Error(), "private-command-output") {
		t.Fatalf("missing runtime did not produce safe, recoverable guidance: %v", err)
	}
}

func TestCommandParsingAndHelp(t *testing.T) {
	var loaded []string
	capture := &app{stdout: io.Discard, stderr: io.Discard, stdin: strings.NewReader(""), load: func(path string) (config.Config, error) {
		loaded = append(loaded, path)
		return config.Config{}, errors.New("stop after load")
	}}
	if err := capture.execute(t.Context(), []string{"snapshot", "x", "--config", "/tmp/config.json", "--name", "golden"}); err == nil || !slices.Equal(loaded, []string{"/tmp/config.json"}) {
		t.Fatalf("interspersed --config was not honored: %v %v", loaded, err)
	}
	t.Setenv("T3FLEET_CONFIG", "/tmp/from-env.json")
	capture.configPath = ""
	if err := capture.execute(t.Context(), []string{"list"}); err == nil || loaded[len(loaded)-1] != "/tmp/from-env.json" {
		t.Fatalf("T3FLEET_CONFIG was not honored: %v", loaded)
	}
	if err := run(t.Context(), []string{"--config"}, io.Discard, io.Discard); err == nil {
		t.Fatal("missing config value accepted")
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"help"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "serve") {
		t.Fatal("help unavailable")
	}
	out.Reset()
	if err := run(t.Context(), []string{"version"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "t3fleet") {
		t.Fatal("version unavailable")
	}
}

type readyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func TestServeLifecycleAndPrivateState(t *testing.T) {
	t.Setenv("BOAT_API_KEY", "fixture-only-key")
	cfg := config.Default()
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	cfg.Providers.Boat = &config.Boat{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := &readyWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ready) }()
	select {
	case <-ready.ready:
	case err := <-done:
		t.Fatalf("serve exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not become ready")
	}
	token, err := readAdminToken(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := callAdmin(t.Context(), cfg.Listen, token, "GET", "/v1/machines", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-only-key") {
		t.Fatal("list leaked account credentials")
	}
	for _, path := range []string{"/_healthcheck", "/_info"} {
		if _, err := callAdmin(t.Context(), cfg.Listen, "", "GET", path, nil); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not shut down")
	}
	reopened, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatalf("serve retained state ownership: %v", err)
	}
	_ = reopened.Close()
}

func TestCLIInvalidCommandsDoNotMutate(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	for _, args := range [][]string{
		{"list", "extra"}, {"inspect"}, {"start", "--bad"}, {"inspect", "../x"}, {"inspect", "x", "extra"},
		{"create", "--name", "demo"}, {"create", "--name", "demo", "--provider", "boat", "extra"},
		{"snapshot", "x"}, {"snapshot", "x", "--name", "snap", "extra"}, {"unknown"},
	} {
		if err := runClient(t.Context(), cfg, args[0], args[1:], io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid command %v", args)
		}
	}
	if err := runClient(t.Context(), cfg, "list", nil, io.Discard, io.Discard); err == nil {
		t.Fatal("missing daemon token accepted")
	}
	for _, args := range [][]string{{"unknown"}, {"--config"}, {"--config", "/missing/config.json", "list"}} {
		if err := run(t.Context(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := run(t.Context(), nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCreateRetainsManagedMachineID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "operation_failed", "machine": map[string]string{"id": id}})
	}))
	defer server.Close()
	_, err := callAdmin(t.Context(), strings.TrimPrefix(server.URL, "http://"), "token", "POST", "/v1/machines", map[string]string{"name": "demo", "provider": "boat"})
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "remains managed") {
		t.Fatalf("missing recovery ID: %v", err)
	}
}

func TestClientRejectsRedirectsAndMalformedResponses(t *testing.T) {
	forwarded := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	_, err := callAdmin(t.Context(), strings.TrimPrefix(redirect.URL, "http://"), "secret-token", "GET", "/v1/machines", nil)
	if err == nil || forwarded {
		t.Fatal("daemon redirect forwarded credentials")
	}
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1)) }))
	defer invalid.Close()
	if _, err := callAdmin(t.Context(), strings.TrimPrefix(invalid.URL, "http://"), "token", "GET", "/v1/machines", nil); err == nil {
		t.Fatal("oversized response accepted")
	}
}

// Batch preparation is an admin-only operation, just like cloud allocation.
type fakeBatchFleet struct {
	fakeFleet
	preparations int
}

func (f *fakeBatchFleet) PrepareBatch(_ context.Context, r fleet.BatchRequest) (fleet.Batch, error) {
	f.preparations++
	return fleet.Batch{ID: r.ID}, nil
}
func (f *fakeBatchFleet) Batch(id string) (fleet.Batch, error) { return fleet.Batch{ID: id}, nil }
func TestBatchAdminAuthAndStrictInput(t *testing.T) {
	f := &fakeBatchFleet{}
	h := newAdminHandler(f, "token")
	for _, path := range []string{"/v1/batches", "/v1/batches/abc", "/v1/machines/one"} {
		for _, method := range []string{"POST", "GET", "DELETE"} {
			if w := adminRequest(h, method, path, "", `{}`); w.Code != 401 {
				t.Fatal("missing auth accepted", path, w.Code)
			}
		}
	}
	if f.preparations != 0 || len(f.calls) != 0 {
		t.Fatal("unauthorized mutation")
	}
	if w := adminRequest(h, "POST", "/v1/batches", "token", `{"id":"x","unknown":true}`); w.Code != 400 {
		t.Fatal("unknown batch field accepted")
	}
	if w := adminRequest(h, "POST", "/v1/batches", "token", `{"id":"x","name":"test","provider":"mock","count":10}`); w.Code != 200 || f.preparations != 1 {
		t.Fatal("batch route failed")
	}
}
