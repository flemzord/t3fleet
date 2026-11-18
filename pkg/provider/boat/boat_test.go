package boat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func clientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := New(Config{APIKey: "test-secret", Organization: "test-org", BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.pollInterval = time.Millisecond
	return c
}

func respond(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(b)
}

func TestCreateFromSnapshot(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/sandboxes" || r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("X-Boat-Org") != "test-org" || r.Header.Get("Idempotency-Key") != "operation-1" {
			t.Errorf("unexpected request metadata: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body["from"] != "golden-t3" || body["name"] != "skiff" || body["visibility"] != "private" || body["type"] != "small" {
			t.Errorf("unexpected body: %#v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		respond(t, w, map[string]any{"ok": true, "sandbox": sandbox{ID: "bx_test", Name: "skiff", State: "provisioning"}})
	})
	m, err := c.Create(t.Context(), provider.CreateRequest{Name: "skiff", Template: provider.Template{Snapshot: "golden-t3"}, Size: "small", OperationID: "operation-1"})
	if err != nil || m.ID != "bx_test" || m.State != provider.StateStarting {
		t.Fatalf("create: %#v, %v", m, err)
	}
}

func TestCreateRequiresSnapshotAndIdempotency(t *testing.T) {
	c := clientFor(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached network") })
	for _, req := range []provider.CreateRequest{
		{OperationID: "op"},
		{Template: provider.Template{Snapshot: "golden"}},
		{Template: provider.Template{Snapshot: "a", Image: "b"}, OperationID: "op"},
		{Template: provider.Template{Snapshot: "golden"}, OperationID: "op", Size: "gigantic"},
	} {
		if _, err := c.Create(t.Context(), req); err == nil {
			t.Error("accepted invalid create")
		}
	}
}

func TestLifecyclePollsVendorStates(t *testing.T) {
	for _, tc := range []struct{ action, transitional, terminal string }{{"resume", "provisioning", "ready"}, {"stop", "archiving", "archived"}} {
		t.Run(tc.action, func(t *testing.T) {
			reads := 0
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if r.URL.Path != "/sandboxes/bx_test/"+tc.action {
						t.Error(r.URL.Path)
					}
					respond(t, w, map[string]any{"ok": true})
					return
				}
				reads++
				state := tc.transitional
				if reads > 1 {
					state = tc.terminal
				}
				respond(t, w, map[string]any{"ok": true, "sandbox": sandbox{ID: "bx_test", State: state}})
			})
			var err error
			if tc.action == "resume" {
				err = c.Start(t.Context(), "bx_test")
			} else {
				err = c.Stop(t.Context(), "bx_test")
			}
			if err != nil || reads != 2 {
				t.Fatalf("transition reads=%d err=%v", reads, err)
			}
		})
	}
}

func TestDeleteConfirmationAndCompletion(t *testing.T) {
	reads := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		status := "processing"
		if r.Method == "DELETE" {
			if r.URL.Path != "/sandboxes/bx_test" || r.Header.Get("X-Ascii-Confirm-Delete") != "bx_test" {
				t.Error("missing exact deletion confirmation")
			}
		} else {
			reads++
			if r.URL.Path != "/deletion-operations/delete-1" {
				t.Error(r.URL.Path)
			}
			status = "completed"
		}
		respond(t, w, map[string]any{"ok": true, "operation": map[string]string{"id": "delete-1", "status": status}})
	})
	if err := c.Delete(t.Context(), "bx_test"); err != nil || reads != 1 {
		t.Fatalf("delete reads=%d err=%v", reads, err)
	}
}

func TestSnapshotWaitsUntilReady(t *testing.T) {
	reads := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		status := "saving"
		if r.Method == "POST" {
			if r.URL.Path != "/saved-snapshots" {
				t.Error(r.URL.Path)
			}
		} else {
			reads++
			if r.URL.Path != "/saved-snapshots/golden-t3" {
				t.Error(r.URL.Path)
			}
			status = "ready"
		}
		respond(t, w, map[string]any{"ok": true, "snapshot": map[string]string{"name": "golden-t3", "snapshotId": "snap_1", "status": status}})
	})
	s, err := c.Snapshot(t.Context(), "bx_test", "golden-t3")
	if err != nil || !s.Ready || s.ID != "golden-t3" || reads != 1 {
		t.Fatalf("snapshot=%#v reads=%d err=%v", s, reads, err)
	}
}

func TestSnapshotFailureDoesNotLeakResponse(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "snapshot": map[string]string{"status": "failed", "error": "secret-token"}})
	})
	_, err := c.Snapshot(t.Context(), "bx_test", "golden-t3")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal(err)
	}
}

func TestPollingHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "snapshot": map[string]string{"status": "saving"}})
		cancel()
	})
	c.pollInterval = time.Hour
	_, err := c.Snapshot(ctx, "bx_test", "golden-t3")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestExecQuotesArgumentsAndEnvironment(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell unavailable")
	}
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string `json:"command"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		// Execute only our test's local harmless printf fixture, using exactly the
		// shell text passed across the provider seam.
		out, err := exec.CommandContext(t.Context(), "sh", "-c", body.Command).Output()
		if err != nil {
			t.Fatal(err)
		}
		respond(t, w, map[string]any{"ok": true, "stdout": string(out), "stderr": "", "exitCode": 0})
	})
	arg := "x'; printf INJECTED; #\n$(printf injected)"
	res, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"sh", "-c", `printf '%s|%s' "$VALUE" "$1"`, "test", arg}, Env: map[string]string{"VALUE": "it's literal; $(nope)"}})
	if err != nil || res.Stdout != "it's literal; $(nope)|"+arg || res.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", res, err)
	}
}

func TestExecReturnsNonzeroExitAndRejectsInvalidInput(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "stdout": "", "stderr": "failure", "exitCode": 7})
	})
	r, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"false"}})
	if err != nil || r.ExitCode != 7 {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	for _, req := range []provider.ExecRequest{{}, {Args: []string{"x\x00y"}}, {Args: []string{"true"}, Env: map[string]string{"BAD=KEY": "value"}}} {
		if _, err := c.Exec(t.Context(), "bx_test", req); err == nil {
			t.Error("accepted invalid exec input")
		}
	}
}

func TestConnectPreservesPrivateToken(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		if r.URL.Path != "/sandboxes/bx_test/host" || body["port"] != float64(3773) || body["public"] != false || body["localhost"] != true {
			t.Errorf("invalid host request: %s", data)
		}
		respond(t, w, map[string]any{"ok": true, "url": "https://machine-3773.on.boat.dev/?_token=port-secret"})
	})
	ep, err := c.Connect(t.Context(), "bx_test", 3773)
	if err != nil || !strings.Contains(ep.URL, "_token=port-secret") || len(ep.Headers) != 0 {
		t.Fatalf("connect failed: %v", err)
	}
}

func TestConnectRefusesPublicOrUnsafeURL(t *testing.T) {
	for _, endpoint := range []string{"https://machine.on.boat.dev", "http://machine.on.boat.dev/?_token=secret", "https://user:pass@machine.on.boat.dev/?_token=secret"} {
		t.Run(endpoint, func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
				respond(t, w, map[string]any{"ok": true, "url": endpoint})
			})
			if _, err := c.Connect(t.Context(), "bx_test", 3773); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("expected safe endpoint rejection, got %v", err)
			}
		})
	}
}

func TestAPIErrorRedactsBodyAndMapsNotFound(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				respond(t, w, map[string]any{"ok": false, "code": "permission_denied", "message": "test-secret https://host/?_token=port-secret"})
			})
			_, err := c.Get(t.Context(), "bx_test")
			if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "port-secret") {
				t.Fatal(err)
			}
			if status == http.StatusNotFound && !errors.Is(err, provider.ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestDoesNotFollowCredentialRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect with credentials") }))
	defer target.Close()
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	if _, err := c.Get(t.Context(), "bx_test"); err == nil {
		t.Fatal("accepted redirect")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	for _, cfg := range []Config{{}, {APIKey: "x", BaseURL: "http://example.com"}, {APIKey: "x", BaseURL: "https://user:pass@boat.dev"}, {APIKey: "x\nsecret"}, {APIKey: "x", BaseURL: "https://boat.dev?secret=1"}} {
		if _, err := New(cfg); err == nil {
			t.Error("accepted invalid configuration")
		}
	}
}

func TestGetRejectsPathTraversalAndInconsistentResponse(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "sandbox": sandbox{ID: "bx_other", State: "ready"}})
	})
	for _, id := range []string{"../bad", "", "bx_test"} {
		if _, err := c.Get(t.Context(), id); err == nil {
			t.Errorf("accepted id %q", id)
		}
	}
}
