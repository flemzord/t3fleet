package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

const serverVersion = "0.1.11-nightly.test"

type mockProvider struct {
	mu              sync.Mutex
	server          *httptest.Server
	machines        map[string]provider.Machine
	operations      map[string]string
	requests        []provider.CreateRequest
	allocations     int
	starts          int
	execs           int
	createFailsOnce bool
	execFails       bool
	execExitCode    int
	deleteFails     bool
	version         string
}

func newProvider(t *testing.T) *mockProvider {
	t.Helper()
	p := &mockProvider{machines: map[string]provider.Machine{}, operations: map[string]string{}, version: serverVersion}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-token" || r.Header.Get("X-Provider-Key") != "private-transport" || r.URL.Query().Get("_token") != "provider-secret" {
			t.Error("private endpoint did not receive the expected service and transport authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/t3/environment":
			_ = json.NewEncoder(w).Encode(map[string]any{"environmentId": "env-test", "label": "upstream", "orchestrationProtocolVersion": 2, "serverVersion": serverVersion, "directEndpoints": []string{"https://private.invalid"}})
		case "/api/auth/session":
			_ = json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"orchestration:read", "orchestration:write"}, "permissions": map[string]any{"filesystem": "read-write"}})
		case "/api/probe":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (*mockProvider) Name() string { return "mock" }
func (p *mockProvider) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	if err := ctx.Err(); err != nil {
		return provider.Machine{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	id := p.operations[req.OperationID]
	if id == "" {
		p.allocations++
		id = fmt.Sprintf("remote-%d", p.allocations)
		p.operations[req.OperationID] = id
		p.machines[id] = provider.Machine{ID: id, Name: req.Name, State: provider.StateRunning}
	}
	if p.createFailsOnce {
		p.createFailsOnce = false
		return provider.Machine{}, errors.New("ambiguous network failure after creation")
	}
	return p.machines[id], nil
}
func (p *mockProvider) Get(ctx context.Context, id string) (provider.Machine, error) {
	if err := ctx.Err(); err != nil {
		return provider.Machine{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[id]
	if !ok {
		return provider.Machine{}, provider.ErrNotFound
	}
	return m, nil
}
func (p *mockProvider) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[id]
	if !ok {
		return provider.ErrNotFound
	}
	p.starts++
	m.State = provider.StateRunning
	p.machines[id] = m
	return nil
}
func (p *mockProvider) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[id]
	if !ok {
		return provider.ErrNotFound
	}
	m.State = provider.StateStopped
	p.machines[id] = m
	return nil
}
func (p *mockProvider) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteFails {
		return errors.New("delete unavailable")
	}
	delete(p.machines, id)
	return nil
}
func (p *mockProvider) Exec(ctx context.Context, id string, req provider.ExecRequest) (provider.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.ExecResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.machines[id]; !ok {
		return provider.ExecResult{}, provider.ErrNotFound
	}
	p.execs++
	if p.execExitCode != 0 {
		return provider.ExecResult{ExitCode: p.execExitCode, Stderr: "private command details must remain hidden"}, nil
	}
	if p.execFails {
		return provider.ExecResult{ExitCode: 1, Stderr: "must-not-be-exposed"}, nil
	}
	if strings.Join(req.Args, " ") != "/bin/bash -lc exec t3fleet-runtime-info" {
		return provider.ExecResult{}, errors.New("unexpected provisioning command")
	}
	data, _ := json.Marshal(map[string]string{"token": "service-token", "environmentId": "env-test", "serverVersion": p.version})
	return provider.ExecResult{Stdout: string(data)}, nil
}
func (p *mockProvider) Connect(ctx context.Context, id string, port int) (provider.Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return provider.Endpoint{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.machines[id].State != provider.StateRunning || port != 3773 {
		return provider.Endpoint{}, errors.New("machine endpoint is not available")
	}
	return provider.Endpoint{URL: p.server.URL + "?_token=provider-secret", Headers: http.Header{"X-Provider-Key": {"private-transport"}}}, nil
}

type fixture struct {
	m       *fleet.Manager
	store   *state.Store
	dir     string
	p       *mockProvider
	options fleet.Options
}

func newFleet(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{p: newProvider(t), dir: filepath.Join(t.TempDir(), "private-state")}
	var err error
	f.store, err = state.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.options = fleet.Options{Providers: map[string]provider.Provider{"mock": f.p}, Templates: map[string]provider.Template{"mock": {Snapshot: "golden"}}, Store: f.store, ExpectedVersion: serverVersion, ReadyTimeout: time.Second}
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.m != nil {
			_ = f.m.Close(context.Background())
		}
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	return f
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = state.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.options.Store = f.store
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
}

func create(t *testing.T, f *fixture, name string) fleet.Machine {
	t.Helper()
	m, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: name, Provider: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "ready" || m.RemoteID == "" || m.URL == "" {
		t.Fatalf("created machine is not ready: %#v", m)
	}
	return m
}

func request(t *testing.T, target, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
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
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func pair(t *testing.T, f *fixture, machine fleet.Machine) string {
	t.Helper()
	link, err := f.m.Pair(t.Context(), machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.PostForm(machine.URL+"/oauth/token", url.Values{"subject_token": {fragment.Get("token")}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var data struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&data) != nil || data.AccessToken == "" {
		t.Fatal("pairing did not issue a local client credential")
	}
	if data.AccessToken == "service-token" {
		t.Fatal("upstream service token was returned to the client")
	}
	return data.AccessToken
}

func TestLifecyclePairingWakeAndDurableRestart(t *testing.T) {
	f := newFleet(t)
	machine := create(t, f, "workbench")
	status, b := request(t, machine.URL+"/.well-known/t3/environment", "")
	if status != http.StatusOK || !strings.Contains(string(b), `"label":"workbench"`) || strings.Contains(string(b), "directEndpoints") {
		t.Fatalf("descriptor %d: %s", status, b)
	}
	if status, _ := request(t, machine.URL+"/api/probe", ""); status != http.StatusUnauthorized {
		t.Fatalf("unpaired request status: %d", status)
	}
	token := pair(t, f, machine)
	if status, _ := request(t, machine.URL+"/api/probe", token); status != http.StatusOK {
		t.Fatalf("paired request status: %d", status)
	}
	if err := f.m.Stop(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.m.Get(machine.ID); m.State != "stopped" {
		t.Fatalf("stop state: %s", m.State)
	}
	if status, _ := request(t, machine.URL+"/api/probe", "invalid-client"); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized request to stopped machine: %d", status)
	}
	f.p.mu.Lock()
	unauthorizedStarts := f.p.starts
	f.p.mu.Unlock()
	if unauthorizedStarts != 0 {
		t.Fatal("unauthorized request woke a billable machine")
	}
	if status, _ := request(t, machine.URL+"/api/probe", token); status != http.StatusOK {
		t.Fatalf("automatic wake status: %d", status)
	}
	f.p.mu.Lock()
	starts := f.p.starts
	f.p.mu.Unlock()
	if starts != 1 {
		t.Fatalf("automatic wake started provider %d times", starts)
	}
	f.restart(t)
	reloaded, err := f.m.Get(machine.ID)
	if err != nil || reloaded.URL != machine.URL || reloaded.RemoteID != machine.RemoteID {
		t.Fatalf("stable identity/endpoint after restart: %#v, %v", reloaded, err)
	}
	if status, _ := request(t, reloaded.URL+"/api/probe", token); status != http.StatusOK {
		t.Fatalf("persisted client credential status: %d", status)
	}
	if err := f.m.Delete(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Get(machine.ID); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatalf("deleted machine: %v", err)
	}
}

func TestValidationAndDuplicateNames(t *testing.T) {
	f := newFleet(t)
	for _, req := range []fleet.CreateRequest{{Name: "Bad Name", Provider: "mock"}, {Name: "valid", Provider: "unknown"}} {
		if _, err := f.m.Create(t.Context(), req); err == nil {
			t.Fatalf("invalid create succeeded: %#v", req)
		}
	}
	existing := create(t, f, "same-name")
	if duplicate, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "same-name", Provider: "mock"}); !errors.Is(err, fleet.ErrConflict) || duplicate.ID != existing.ID {
		t.Fatalf("duplicate must identify the existing record: machine=%#v err=%v", duplicate, err)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.allocations != 1 {
		t.Fatalf("invalid/duplicate requests allocated %d machines", f.p.allocations)
	}
}

func TestFailedCreationReservesOnlyItsOwnName(t *testing.T) {
	f := newFleet(t)
	f.p.createFailsOnce = true
	original, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "reserved-name", Provider: "mock"})
	if err == nil || original.State != "create-failed" {
		t.Fatal("expected an interrupted creation")
	}
	duplicate, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "reserved-name", Provider: "mock", Template: provider.Template{Snapshot: "different-base"}})
	if !errors.Is(err, fleet.ErrConflict) || duplicate.ID != original.ID || duplicate.State != "create-failed" {
		t.Fatalf("conflict discarded the failed reservation identity: %#v %v", duplicate, err)
	}
	create(t, f, "another-name")
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if len(f.p.requests) != 2 {
		t.Fatal("duplicate name reached the provider or prevented an independent machine")
	}
}

func TestPreparedTemplateIsRequired(t *testing.T) {
	f := newFleet(t)
	f.options.Templates = nil
	f.restart(t)
	if _, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "no-image", Provider: "mock"}); err == nil {
		t.Fatal("unprepared machine creation succeeded")
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.allocations != 0 {
		t.Fatal("missing image allocated a billable machine")
	}
}

func TestMissingRuntimeIsDistinguishableAndKeepsAllocatedMachine(t *testing.T) {
	f := newFleet(t)
	f.p.execExitCode = 127
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "unprepared-base", Provider: "mock"})
	if !errors.Is(err, fleet.ErrRuntimeUnavailable) || machine.RemoteID == "" || machine.State != "not-ready" {
		t.Fatalf("missing runtime was not classified with a recoverable machine: %#v %v", machine, err)
	}
	if strings.Contains(err.Error(), "private command") {
		t.Fatal("runtime error exposed command output")
	}
	if err := f.m.Delete(t.Context(), machine.ID); err != nil {
		t.Fatalf("unprepared VM could not be cleaned up: %v", err)
	}
}

func TestVersionMismatchRemainsRecoverable(t *testing.T) {
	f := newFleet(t)
	f.p.version = "wrong-version"
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "versioned", Provider: "mock"})
	if err == nil || !strings.Contains(err.Error(), "version") || machine.ID == "" || machine.RemoteID == "" || machine.State != "not-ready" {
		t.Fatalf("version mismatch: %#v, %v", machine, err)
	}
	f.restart(t)
	f.p.mu.Lock()
	f.p.version = serverVersion
	f.p.mu.Unlock()
	if err := f.m.Start(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.m.Get(machine.ID); m.State != "ready" {
		t.Fatalf("recovered state: %s", m.State)
	}
}

func TestAmbiguousCreateReconcilesDurableOperation(t *testing.T) {
	f := newFleet(t)
	f.p.createFailsOnce = true
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "recover-create", Provider: "mock"})
	if err == nil || machine.ID == "" || machine.State != "create-failed" {
		t.Fatalf("ambiguous creation: %#v, %v", machine, err)
	}
	if err := f.m.Delete(t.Context(), machine.ID); err == nil {
		t.Fatal("pending creation was forgotten without reconciling the remote resource")
	}
	f.restart(t)
	if err := f.m.Start(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.allocations != 1 || len(f.p.requests) != 2 || f.p.requests[0].OperationID != f.p.requests[1].OperationID || f.p.requests[0].OperationID != machine.ID {
		t.Fatalf("creation was not reconciled by durable operation: allocations=%d requests=%#v", f.p.allocations, f.p.requests)
	}
}

func TestFailedInitializationAndDeleteRetainState(t *testing.T) {
	f := newFleet(t)
	f.p.execFails = true
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "recover-init", Provider: "mock"})
	if err == nil || machine.State != "not-ready" || machine.RemoteID == "" || strings.Contains(err.Error(), "must-not-be-exposed") {
		t.Fatalf("failed initialization: %#v, %v", machine, err)
	}
	f.restart(t)
	f.p.mu.Lock()
	f.p.execFails = false
	f.p.deleteFails = true
	f.p.mu.Unlock()
	if err := f.m.Start(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(t.Context(), machine.ID); err == nil {
		t.Fatal("failed provider deletion succeeded")
	}
	f.restart(t)
	if m, err := f.m.Get(machine.ID); err != nil || m.RemoteID != machine.RemoteID {
		t.Fatalf("failed deletion lost durable resource: %#v, %v", m, err)
	}
	f.p.mu.Lock()
	f.p.deleteFails = false
	f.p.mu.Unlock()
	if err := f.m.Delete(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentStartsSerializeRemoteWake(t *testing.T) {
	f := newFleet(t)
	machine := create(t, f, "parallel")
	if err := f.m.Stop(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := f.m.Start(t.Context(), machine.ID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.starts != 1 {
		t.Fatalf("concurrent callers started provider %d times", f.p.starts)
	}
}

// A provider can allocate a snapshot clone before its command service accepts
// work. This is a real Boat API state, not an executable-image failure.
type delayedProvider struct {
	*mockProvider
	readinessMu sync.Mutex
	remaining   int
}

func (p *delayedProvider) Get(ctx context.Context, id string) (provider.Machine, error) {
	remote, err := p.mockProvider.Get(ctx, id)
	if err != nil {
		return remote, err
	}
	p.readinessMu.Lock()
	defer p.readinessMu.Unlock()
	if p.remaining > 0 {
		p.remaining--
		remote.State = provider.StateStarting
	}
	return remote, nil
}
func (p *delayedProvider) Exec(ctx context.Context, id string, req provider.ExecRequest) (provider.ExecResult, error) {
	p.readinessMu.Lock()
	remaining := p.remaining
	p.readinessMu.Unlock()
	if remaining > 0 {
		return provider.ExecResult{}, errors.New("command service is still provisioning")
	}
	return p.mockProvider.Exec(ctx, id, req)
}
func TestCreateWaitsForProviderSnapshotRestore(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	delayed := &delayedProvider{mockProvider: f.p, remaining: 3}
	f.options.Providers = map[string]provider.Provider{"mock": delayed}
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "slow-restore", Provider: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	if machine.State != "ready" {
		t.Fatalf("state=%s", machine.State)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.execs != 1 || f.p.allocations != 1 {
		t.Fatalf("execs=%d allocations=%d", f.p.execs, f.p.allocations)
	}
}
func TestRestoreCancellationRetainsAllocatedIdentity(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	delayed := &delayedProvider{mockProvider: f.p, remaining: 100}
	f.options.Providers = map[string]provider.Provider{"mock": delayed}
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	machine, err := f.m.Create(ctx, fleet.CreateRequest{Name: "interrupted-restore", Provider: "mock"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	saved, getErr := f.m.Get(machine.ID)
	if getErr != nil || saved.RemoteID == "" || saved.State != "not-ready" {
		t.Fatalf("allocated identity lost: %#v %v", saved, getErr)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.execs != 0 {
		t.Fatal("executed runtime before provider readiness")
	}
}

func TestActiveTrafficStillRevalidatesServiceAuthorization(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var clockMu sync.Mutex
	now := time.Now()
	f.options.Now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	machine := create(t, f, "active-machine")
	token := pair(t, f, machine)
	f.p.mu.Lock()
	before := f.p.execs
	f.p.mu.Unlock()
	for range 5 {
		clockMu.Lock()
		now = now.Add(3 * time.Minute)
		clockMu.Unlock()
		code, _ := request(t, machine.URL+"/api/probe", token)
		if code != http.StatusOK {
			t.Fatalf("active request HTTP%d", code)
		}
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.execs <= before {
		t.Fatal("continuous traffic postponed service authorization refresh indefinitely")
	}
}

type missingOnDelete struct{ *mockProvider }

func (p *missingOnDelete) Delete(context.Context, string) error { return provider.ErrNotFound }
func TestAlreadyDeletedRemoteConvergesLocalDeletion(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.options.Providers = map[string]provider.Provider{"mock": &missingOnDelete{f.p}}
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	machine := create(t, f, "already-deleted")
	if err := f.m.Delete(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Get(machine.ID); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatalf("stale local machine remains: %v", err)
	}
}

// Namespace can allocate a Devbox successfully and then fail its activation.
// A returned provider identity is authoritative even when Create also reports
// an error: retaining it makes cleanup and restart independent of replaying a
// billable creation request.
type partialIdentityProvider struct {
	*mockProvider
	failureMu      sync.Mutex
	calls          int
	failures       int
	identityOnCall int
}

func (p *partialIdentityProvider) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	remote, err := p.mockProvider.Create(ctx, req)
	if err != nil {
		return remote, err
	}
	p.failureMu.Lock()
	defer p.failureMu.Unlock()
	p.calls++
	if p.calls <= p.failures {
		if p.calls < p.identityOnCall {
			remote = provider.Machine{}
		}
		return remote, errors.New("activation failed after provider allocation")
	}
	return remote, nil
}

func TestCreatePartialIdentitySurvivesFailureAndAllowsCleanup(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	partial := &partialIdentityProvider{mockProvider: f.p, failures: 1, identityOnCall: 1}
	f.options.Providers = map[string]provider.Provider{"mock": partial}
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "partial-identity", Provider: "mock"})
	if err == nil || machine.ID == "" || machine.RemoteID == "" {
		t.Fatalf("partially allocated identity was lost: %#v, %v", machine, err)
	}
	f.restart(t)
	persisted, err := f.m.Get(machine.ID)
	if err != nil || persisted.RemoteID != machine.RemoteID {
		t.Fatalf("partial identity was not durable: %#v, %v", persisted, err)
	}
	if err := f.m.Delete(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if len(f.p.requests) != 1 || len(f.p.machines) != 0 {
		t.Fatalf("cleanup replayed creation or left its resource: requests=%d machines=%d", len(f.p.requests), len(f.p.machines))
	}
}

func TestReconciledPartialIdentityAvoidsAnotherCreateAfterRestart(t *testing.T) {
	f := newFleet(t)
	if err := f.m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	partial := &partialIdentityProvider{mockProvider: f.p, failures: 2, identityOnCall: 2}
	f.options.Providers = map[string]provider.Provider{"mock": partial}
	var err error
	f.m, err = fleet.New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.m.Create(t.Context(), fleet.CreateRequest{Name: "reconciled-identity", Provider: "mock"})
	if err == nil || machine.ID == "" || machine.RemoteID != "" {
		t.Fatalf("expected initial ambiguous creation: %#v, %v", machine, err)
	}
	f.restart(t)
	if err := f.m.Start(t.Context(), machine.ID); err == nil {
		t.Fatal("partial reconciliation unexpectedly succeeded")
	}
	persisted, err := f.m.Get(machine.ID)
	if err != nil || persisted.RemoteID == "" {
		t.Fatalf("reconciliation discarded returned identity: %#v, %v", persisted, err)
	}
	f.restart(t)
	if err := f.m.Start(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := f.m.Get(machine.ID)
	if err != nil || ready.State != "ready" || ready.RemoteID != persisted.RemoteID {
		t.Fatalf("restart failed to reuse partial identity: %#v, %v", ready, err)
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if len(f.p.requests) != 2 || f.p.allocations != 1 {
		t.Fatalf("restart unnecessarily replayed creation: requests=%d allocations=%d", len(f.p.requests), f.p.allocations)
	}
}
