package namespace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

// Synthetic Namespace token for testing account routing; the loopback
// server substitutes for Namespace authentication.
func fixtureToken(tenant, actor string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload, _ := json.Marshal(map[string]string{"tenant_id": tenant, "actor_id": actor})
	return "nsct_" + header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}

const testTenant = "test-tenant"

type testToken struct{}

func (testToken) IssueToken(context.Context, time.Duration, bool) (string, error) {
	return fixtureToken(testTenant, "tester"), nil
}

func newTestClient(t *testing.T, handler http.HandlerFunc, dial func(context.Context, SSHAuth) (Connection, error)) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{TenantID: testTenant, Endpoint: server.URL, TokenSource: testToken{}, DialSSH: dial})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func reply(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
func body(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCreateAndReconcileOperation(t *testing.T) {
	var mu sync.Mutex
	var stored map[string]any
	creates := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureToken(testTenant, "tester") {
			t.Error("missing bearer")
		}
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Error("missing Connect protocol")
		}
		mu.Lock()
		defer mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, servicePath) {
		case "Fetch":
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			reply(t, w, map[string]any{"devbox": stored, "instanceId": "instance-1"})
		case "Create":
			input := body(t, r)
			if input["imageRef"] != "registry.example/t3@sha256:abc" || input["activate"] != true || input["site"] != "iad" {
				t.Errorf("bad create input: %#v", input)
			}
			if _, ok := input["versionControl"].(map[string]any); !ok {
				t.Error("must disable inherited checkout")
			}
			shape := input["instanceShape"].(map[string]any)
			if shape["virtualCpu"] != float64(8) {
				t.Error("size was not resolved")
			}
			stored = map[string]any{"id": "devbox-1", "name": input["name"], "labels": input["labels"]}
			creates++
			reply(t, w, map[string]any{"devbox": stored, "instanceId": "instance-1"})
		default:
			t.Errorf("unexpected method %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}, nil)
	req := provider.CreateRequest{Name: "fleet-test", OperationID: "op-1", Size: "m", Template: provider.Template{Image: "registry.example/t3@sha256:abc"}}
	machine, err := client.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if machine.ID != "devbox-1" || machine.State != provider.StateRunning {
		t.Fatalf("unexpected machine: %+v", machine)
	}
	again, err := client.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if again != machine || creates != 1 {
		t.Fatalf("retry created another machine: creates=%d", creates)
	}
	req.OperationID = "other"
	if _, err := client.Create(t.Context(), req); err == nil {
		t.Fatal("adopted another operation's Devbox")
	}
}

func TestCreateUsesReservedShapeAfterChangingProfile(t *testing.T) {
	want := provider.Shape{VirtualCPU: 12, MemoryMB: 24576, Architecture: "arm64"}
	first, err := New(Config{TenantID: testTenant, VirtualCPU: want.VirtualCPU, MemoryMB: want.MemoryMB, Architecture: want.Architecture, TokenSource: testToken{}})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := first.ResolveShape("")
	if err != nil || reserved != want {
		t.Fatalf("custom profile resolved to %v, %v", reserved, err)
	}
	rotatedToken := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(rotatedToken, []byte(`{"bearer_token":"`+fixtureToken(testTenant, "instance")+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NSC_TOKEN_FILE", filepath.Join(t.TempDir(), "not-selected"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureToken(testTenant, "instance") {
			t.Error("wrong Namespace credential")
		}
		switch strings.TrimPrefix(r.URL.Path, servicePath) {
		case "Fetch":
			w.WriteHeader(http.StatusNotFound)
		case "Create":
			s := body(t, r)["instanceShape"].(map[string]any)
			if s["virtualCpu"] != float64(want.VirtualCPU) || s["memoryMegabytes"] != float64(want.MemoryMB) || s["machineArch"] != want.Architecture {
				t.Errorf("used current defaults instead of the reservation: %v", s)
			}
			reply(t, w, map[string]any{"devbox": map[string]string{"id": "devbox-1", "name": "worker"}, "instanceId": "inst-1"})
		default:
			t.Errorf("unexpected Namespace method %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	next, err := New(Config{TenantID: testTenant, Endpoint: srv.URL, VirtualCPU: 16, MemoryMB: 32768, Architecture: "amd64", TokenFile: rotatedToken})
	if err != nil {
		t.Fatal(err)
	}
	request := provider.CreateRequest{Name: "worker", OperationID: "op", Shape: reserved, Template: provider.Template{Image: "registered:org/base"}}
	if _, err = next.Create(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Shape.MemoryMB = -1
	if _, err = next.Create(t.Context(), request); err == nil {
		t.Fatal("invalid reserved shape accepted")
	}
}

func TestSnapshotForkAndLifecycle(t *testing.T) {
	methods := []string{}
	running := true
	deleted := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, servicePath)
		methods = append(methods, method)
		input := body(t, r)
		box := map[string]any{"id": "dbx1", "name": "fork"}
		switch method {
		case "Fetch":
			if len(methods) == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if deleted {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			res := map[string]any{"devbox": box}
			if running {
				res["instanceId"] = "inst"
			}
			reply(t, w, res)
		case "CreateFromDevbox":
			if input["fromDevbox"] != "golden" || input["snapshotId"] != "snapshot-ready" {
				t.Error("wrong snapshot source")
			}
			reply(t, w, map[string]any{"devbox": box, "instanceId": "inst"})
		case "Stop":
			if input["name"] != "fork" {
				t.Error("stop uses resolved name")
			}
			running = false
			reply(t, w, map[string]any{})
		case "Activate":
			if input["waitForReadiness"] != true {
				t.Error("must wait for readiness")
			}
			running = true
			reply(t, w, map[string]any{"devbox": box, "instanceId": "inst"})
		case "Expire":
			if input["name"] != "fork" {
				t.Error("delete uses resolved name")
			}
			deleted = true
			reply(t, w, map[string]any{})
		case "ListSnapshots":
			reply(t, w, map[string]any{"snapshots": []map[string]any{{"id": "ready", "completedSnapshot": "complete"}, {"id": "pending"}, {"id": "abandoned", "completedSnapshot": "complete", "abandonedAt": "2026-01-01T00:00:00Z"}}})
		default:
			t.Errorf("unknown method %s", method)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}, nil)
	machine, err := client.Create(t.Context(), provider.CreateRequest{Name: "fork", OperationID: "op", Template: provider.Template{Source: "golden", Snapshot: "snapshot-ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Stop(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	got, err := client.Get(t.Context(), machine.ID)
	if err != nil || got.State != provider.StateStopped {
		t.Fatalf("state: %+v %v", got, err)
	}
	if err = client.Start(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	snapshots, err := client.ListSnapshots(t.Context(), machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 3 || !snapshots[0].Ready || snapshots[1].Ready || snapshots[2].Ready {
		t.Fatalf("snapshot readiness: %+v", snapshots)
	}
	if err = client.Delete(t.Context(), machine.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(client).(provider.Snapshotter); ok {
		t.Fatal("must not claim unsupported manual capture")
	}
}

type testConnection struct {
	closed  int
	address string
	exec    provider.ExecRequest
}

func (c *testConnection) Exec(ctx context.Context, req provider.ExecRequest) (provider.ExecResult, error) {
	if ctx.Err() != nil {
		return provider.ExecResult{}, ctx.Err()
	}
	c.exec = req
	return provider.ExecResult{Stdout: "ready", ExitCode: 3}, nil
}
func (c *testConnection) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	c.address = address
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}
func (c *testConnection) Close() error { c.closed++; return nil }
func TestPrivateExecAndTunnelLifetime(t *testing.T) {
	connections := []*testConnection{}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		input := body(t, r)
		if input["includeSshCredentials"] != true {
			t.Error("SSH credentials not requested")
		}
		reply(t, w, map[string]any{"devbox": map[string]any{"id": "dbx", "name": "test", "mainUser": "devbox"}, "instanceId": "instance", "privateSshKey": []byte("test-key"), "instanceMetadataSummary": map[string]any{"ingressDomain": "example.com"}})
	}, func(_ context.Context, a SSHAuth) (Connection, error) {
		if a.User != "devbox" || a.InstanceID != "instance" || string(a.PrivateKey) != "test-key" {
			t.Error("wrong activation credentials")
		}
		conn := &testConnection{}
		connections = append(connections, conn)
		return conn, nil
	})
	result, err := client.Exec(t.Context(), "dbx", provider.ExecRequest{Args: []string{"t3", "--version"}})
	if err != nil || result.ExitCode != 3 || result.Stdout != "ready" || connections[0].closed != 1 {
		t.Fatalf("exec result/cleanup: %+v %v", result, err)
	}
	setup, cancel := context.WithCancel(t.Context())
	endpoint, err := client.Connect(setup, "dbx", 3773)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	tunnel, err := endpoint.DialContext(t.Context(), "tcp", "attacker.example:80")
	if err != nil {
		t.Fatal(err)
	}
	_ = tunnel.Close()
	if connections[1].address != "127.0.0.1:3773" {
		t.Fatal("tunnel dial escaped configured port")
	}
	if connections[1].closed != 0 {
		t.Fatal("setup context owns returned endpoint")
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if connections[1].closed != 1 {
		t.Fatal("endpoint did not close tunnel")
	}
}

func TestControlPlaneCancellationAndRedaction(t *testing.T) {
	started := make(chan struct{})
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := client.Get(ctx, "dbx"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted cancellation: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "secret-body")
	}))
	defer server.Close()
	client.config.Endpoint = server.URL
	_, err := client.Get(t.Context(), "dbx")
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestRejectCredentialRedirect(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}, nil)
	if _, err := client.Get(t.Context(), "dbx"); err == nil {
		t.Fatal("accepted redirect")
	}
	if reached {
		t.Fatal("followed credential-bearing redirect")
	}
}
func TestValidateBeforeMutation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached API") }, nil)
	cases := []provider.CreateRequest{
		{Name: "bad name", OperationID: "op", Template: provider.Template{Image: "image"}},
		{Name: "test", Template: provider.Template{Image: "image"}},
		{Name: "test", OperationID: "op", Template: provider.Template{Snapshot: "snapshot"}},
		{Name: "test", OperationID: "op", Template: provider.Template{Image: "image", Source: "golden"}},
	}
	for _, req := range cases {
		if _, err := client.Create(t.Context(), req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
}
func TestShellCommandQuotesData(t *testing.T) {
	command, err := shellCommand(provider.ExecRequest{Args: []string{"printf", "%s", "$(touch /bad);a'b"}, Env: map[string]string{"KEY": "a b"}})
	if err != nil {
		t.Fatal(err)
	}
	if command != "env 'KEY=a b' 'printf' '%s' '$(touch /bad);a'\\''b'" {
		t.Fatalf("unexpected command: %s", command)
	}
	if _, err = shellCommand(provider.ExecRequest{Args: []string{"true"}, Env: map[string]string{"KEY;touch bad": "v"}}); err == nil {
		t.Fatal("accepted invalid environment key")
	}
}

// Namespace's stdlib.Label contract excludes slash-separated Kubernetes-style
// names. Keep this independent of the adapter's validation implementation.
func TestCreateLabelsFollowNamespaceSchema(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z]([a-z0-9-.]*[a-z0-9])?$`)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Fetch") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		input := body(t, r)
		for _, entry := range input["labels"].([]any) {
			item := entry.(map[string]any)
			name := item["name"].(string)
			if len(name) > 63 || !pattern.MatchString(name) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		reply(t, w, map[string]any{"devbox": map[string]any{"id": "dbx-contract", "name": input["name"], "labels": input["labels"]}, "instanceId": "instance-contract"})
	}, nil)
	_, err := client.Create(t.Context(), provider.CreateRequest{Name: "canary", OperationID: "pending-operation", Template: provider.Template{Source: "golden", Snapshot: "ready-snapshot"}, Labels: map[string]string{"t3fleet-id": "pending-operation"}})
	if err != nil {
		t.Fatalf("provider rejected operation labels: %v", err)
	}
}

func TestInvalidLabelsDoNotReachControlPlane(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid label reached control plane") }, nil)
	cases := []map[string]string{
		{"kubernetes.io/name": "value"},
		{"Uppercase": "value"},
		{"_underscore": "value"},
		{strings.Repeat("a", 64): "value"},
		{"name": strings.Repeat("a", 256)},
		{operationLabel: "spoofed-operation"},
	}
	for _, labels := range cases {
		_, err := client.Create(t.Context(), provider.CreateRequest{Name: "test", OperationID: "operation", Template: provider.Template{Image: "image"}, Labels: labels})
		if err == nil {
			t.Fatalf("accepted invalid labels: %v", labels)
		}
	}
}

func TestDeleteRequiresObservedFinalState(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	expired := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Expire") {
			expired = true
			reply(t, w, map[string]any{})
			return
		}
		if expired {
			cancel()
		}
		reply(t, w, map[string]any{"devbox": map[string]any{"id": "deleting", "name": "delete-test"}, "instanceId": "still-running"})
	}, nil)
	err := client.Delete(ctx, "deleting")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete returned without observing deletion: %v", err)
	}
}

func TestDeleteWaitsForAsynchronousCompletion(t *testing.T) {
	for _, terminal := range []string{"not-found", "destroyed"} {
		t.Run(terminal, func(t *testing.T) {
			expired := false
			checks := 0
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/Expire") {
					expired = true
					reply(t, w, map[string]any{})
					return
				}
				box := map[string]any{"id": "deleting", "name": "delete-test"}
				if expired {
					checks++
					if checks == 2 {
						if terminal == "not-found" {
							w.WriteHeader(http.StatusNotFound)
							return
						}
						box["destroyedAt"] = "2026-11-18T00:00:00Z"
					}
				}
				reply(t, w, map[string]any{"devbox": box, "instanceId": "still-visible"})
			}, nil)
			if err := client.Delete(t.Context(), "deleting"); err != nil {
				t.Fatal(err)
			}
			if checks != 2 {
				t.Fatalf("Delete did not wait for terminal state: %d", checks)
			}
		})
	}
}

func TestDeleteAlreadyAbsentIsIdempotent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/Fetch") {
			t.Errorf("unexpected mutation: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}, nil)
	if err := client.Delete(t.Context(), "already-gone"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRegisteredImageWithSlash(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, servicePath) {
		case "Fetch":
			w.WriteHeader(http.StatusNotFound)
		case "Create":
			input := body(t, r)
			if input["imageName"] != "example-org/t3-worker" || input["imageRef"] != nil {
				t.Errorf("registered image was not sent by name: %+v", input)
			}
			reply(t, w, map[string]any{"devbox": map[string]any{"id": "id", "name": "test"}, "instanceId": "instance-1"})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}, nil)
	req := provider.CreateRequest{Name: "test", OperationID: "op", Template: provider.Template{Image: "registered:example-org/t3-worker"}}
	if _, err := client.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}

type tokenFunc func(context.Context, time.Duration, bool) (string, error)

func (f tokenFunc) IssueToken(ctx context.Context, ttl time.Duration, refresh bool) (string, error) {
	return f(ctx, ttl, refresh)
}

func TestCreateFallbackRequiresConfirmedNoAllocation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		tokenFails bool
	}{
		{"token fails before create", true},
		{"API may have allocated before reporting an activation error", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch strings.TrimPrefix(r.URL.Path, servicePath) {
				case "Fetch":
					w.WriteHeader(http.StatusNotFound)
				case "Create":
					if tt.tokenFails {
						t.Error("a create request was sent despite a local token failure")
					}
					w.WriteHeader(http.StatusTooManyRequests)
					reply(t, w, map[string]string{"code": "resource_exhausted"})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}, nil)
			if tt.tokenFails {
				calls := 0
				client.config.TokenSource = tokenFunc(func(context.Context, time.Duration, bool) (string, error) {
					calls++
					if calls == 1 {
						return fixtureToken(testTenant, "tester"), nil // allow the initial lookup
					}
					return "", errors.New("test token unavailable")
				})
			}
			req := provider.CreateRequest{Name: "test", OperationID: "op", Template: provider.Template{Image: "registered:example-org/t3-worker"}}
			m, err := client.Create(t.Context(), req)
			if m.ID != "" || err == nil || errors.Is(err, provider.ErrCreateRejected) != tt.tokenFails {
				t.Fatalf("unexpected create result: %+v, %v", m, err)
			}
		})
	}
}

func TestTenantPinRejectsReplacedTokenBeforeAnyDevboxRequest(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token.json")
	var seen int
	var lastToken string
	var rotateOnFetch bool
	write := func(token string) error {
		b, err := json.Marshal(map[string]string{"bearer_token": token})
		if err != nil {
			return err
		}
		return os.WriteFile(file, b, 0600)
	}
	// Rotate between the ownership check and create. The second request must
	// re-check the token from the same file instead of trusting its location.
	foreign := fixtureToken("another-tenant", "user")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		lastToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if rotateOnFetch && strings.HasSuffix(r.URL.Path, "/Fetch") {
			if err := write(foreign); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client, err := New(Config{TenantID: testTenant, Endpoint: srv.URL, TokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"malformed-token", fixtureToken("", "user"), foreign} {
		if err := write(token); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Get(t.Context(), "id"); err == nil || errors.Is(err, provider.ErrNotFound) || strings.Contains(err.Error(), token) {
			t.Fatalf("incorrect credential was sent, reported as absence, or exposed: %v", err)
		}
	}
	if seen != 0 {
		t.Fatal("foreign or malformed credentials reached Namespace")
	}
	for _, actor := range []string{"old-local-key", "new-local-key"} {
		token := fixtureToken(testTenant, actor)
		if err := write(token); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Get(t.Context(), "id"); !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("correct tenant was not contacted: %v", err)
		}
		if lastToken != token {
			t.Fatal("credential rotation in same tenant was ignored")
		}
	}
	rotateOnFetch = true
	_, err = client.Create(t.Context(), provider.CreateRequest{Name: "worker", OperationID: "op", Template: provider.Template{Image: "base"}})
	if !errors.Is(err, provider.ErrCreateRejected) || seen != 3 {
		t.Fatalf("tenant changed between fetch and create: issued %d calls, error %v", seen, err)
	}
}
