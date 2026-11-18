package boat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
		if r.Method == "GET" && r.URL.Path == "/sandboxes" {
			if r.URL.Query().Get("label") != "t3fleet-id=operation-1" {
				t.Error("missing durable ownership lookup")
			}
			respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/sandboxes" || r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("X-Boat-Org") != "test-org" || r.Header.Get("Idempotency-Key") != "operation-1" {
			t.Errorf("unexpected request metadata: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body["from"] != "golden-t3" || body["name"] != "skiff" || body["visibility"] != "private" || body["type"] != "small" || body["noEnv"] != true {
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

func TestCreateExplicitEnvironmentOptsIntoCredentials(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}})
			return
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
		}
		if body["environment"] != "t3fleet-user" || body["noEnv"] != false {
			t.Error("explicit environment was not opted in")
		}
		respond(t, w, map[string]any{"ok": true, "sandbox": sandbox{ID: "bx_test", State: "provisioning"}})
	})
	c.environment = "t3fleet-user"
	if _, err := c.Create(t.Context(), provider.CreateRequest{Template: provider.Template{Snapshot: "golden"}, OperationID: "op"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRecoversLostResponseAfterIdempotencyExpiry(t *testing.T) {
	reads := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/sandboxes" || r.URL.Query().Get("label") != "t3fleet-id=old-operation" {
			t.Error("recovery attempted a new billable create")
			w.WriteHeader(500)
			return
		}
		reads++
		if r.URL.Query().Get("cursor") == "" {
			respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}, "pageInfo": map[string]any{"hasMore": true, "nextCursor": "page-2"}})
			return
		}
		respond(t, w, map[string]any{"ok": true, "sandboxes": []sandbox{{ID: "bx_original", Name: "skiff", State: "archived", Labels: map[string]string{"t3fleet-id": "old-operation"}}}, "pageInfo": map[string]any{"hasMore": false, "nextCursor": nil}})
	})
	m, err := c.Create(t.Context(), provider.CreateRequest{Name: "skiff", Template: provider.Template{Snapshot: "golden"}, OperationID: "old-operation"})
	if err != nil || m.ID != "bx_original" || m.State != provider.StateStopped || reads != 2 {
		t.Fatalf("durable recovery failed: machine=%#v reads=%d err=%v", m, reads, err)
	}
}

func TestCreateFailsClosedOnAmbiguousOrIncompleteOwnership(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"ambiguous", map[string]any{"ok": true, "sandboxes": []sandbox{{ID: "bx_one", State: "ready", Labels: map[string]string{"t3fleet-id": "op"}}, {ID: "bx_two", State: "ready", Labels: map[string]string{"t3fleet-id": "op"}}}}},
		{"missing-list", map[string]any{"ok": true}},
		{"missing-label", map[string]any{"ok": true, "sandboxes": []sandbox{{ID: "bx_one", State: "ready"}}}},
		{"cancelled-result", map[string]any{"ok": true, "sandboxes": []sandbox{{ID: "bx_one", State: "cancelled", Labels: map[string]string{"t3fleet-id": "op"}}}}},
		{"broken-pagination", map[string]any{"ok": true, "sandboxes": []any{}, "pageInfo": map[string]any{"hasMore": true, "nextCursor": nil}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("uncertain recovery created a machine")
				}
				respond(t, w, tc.body)
			})
			if _, err := c.Create(t.Context(), provider.CreateRequest{Template: provider.Template{Snapshot: "golden"}, OperationID: "op"}); err == nil {
				t.Error("accepted ambiguous ownership")
			}
		})
	}
}

func TestCreateRejectsOwnershipLookupFailureWithoutPosting(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("lookup failure triggered create")
		}
		w.WriteHeader(503)
		respond(t, w, map[string]any{"ok": false})
	})
	if _, err := c.Create(t.Context(), provider.CreateRequest{Template: provider.Template{Snapshot: "golden"}, OperationID: "op"}); err == nil {
		t.Error("ignored ownership lookup failure")
	}
}

func TestExecErrorDiagnosticsNeverExposeArbitraryCause(t *testing.T) {
	for _, cause := range []error{
		errors.New("TOKENMARKER https://private/?_token=SECRET"),
		&APIError{Status: 502, Code: "tokenmarker"},
		&APIError{Status: 502, Code: "sandbox_restoring"},
		context.Canceled,
	} {
		err := execPhaseError("stdout-read", cause)
		typed, ok := errors.AsType[*ExecError](err)
		if !ok || !strings.Contains(typed.Error(), "stdout-read") || strings.Contains(strings.ToLower(typed.Error()), "tokenmarker") || strings.Contains(typed.Error(), "SECRET") || strings.Contains(typed.Error(), "https://") {
			t.Fatalf("unsafe typed diagnostic: %v", err)
		}
		if !errors.Is(err, cause) {
			t.Error("diagnostic did not preserve error cause")
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

func TestDeleteStopsOnFailedOperation(t *testing.T) {
	reads := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		status := "processing"
		if r.Method != "DELETE" {
			reads++
			status = "failed"
		}
		respond(t, w, map[string]any{"ok": true, "operation": map[string]string{"id": "delete-1", "status": status}})
	})
	err := c.Delete(t.Context(), "bx_test")
	if err == nil || reads != 1 {
		t.Fatalf("failed deletion reads=%d err=%v", reads, err)
	}
}

func TestDeleteBoundsUnfinishedOperation(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		respond(t, w, map[string]any{"ok": true, "operation": map[string]string{"id": "delete-1", "status": "processing"}})
	})
	c.deleteTimeout = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- c.Delete(t.Context(), "bx_test") }()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "in time") {
			t.Fatalf("unfinished deletion err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete kept polling an unfinished operation")
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

type executionFixture struct {
	t             *testing.T
	mu            sync.Mutex
	directory     string
	deleted       bool
	commandStdout string
	commandStderr string
	alterCommand  func(map[string]any)
	alterRead     func(map[string]any)
	onRead        func()
	cleanupStatus int
	commandStatus int
}

var executionDirectory = regexp.MustCompile(`mkdir -m 700 -- '(/tmp/t3fleet-exec-[0-9a-f]{64})'`)

func fixtureClient(t *testing.T) (*Client, *executionFixture) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell unavailable")
	}
	f := &executionFixture{t: t}
	c := clientFor(t, f.serve)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.directory != "" {
			_ = os.RemoveAll(f.directory)
		}
	})
	return c, f
}

func (f *executionFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == "POST" && r.URL.Path == "/sandboxes/bx_test/commands" {
		var body struct {
			Command string `json:"command"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			f.t.Error(err)
			w.WriteHeader(500)
			return
		}
		match := executionDirectory.FindStringSubmatch(body.Command)
		if len(match) != 2 {
			f.t.Error("missing random private directory")
			w.WriteHeader(500)
			return
		}
		f.directory = match[1]
		if f.commandStatus != 0 {
			w.WriteHeader(f.commandStatus)
			respond(f.t, w, map[string]any{"ok": false})
			return
		}
		cmd := exec.CommandContext(f.t.Context(), "sh", "-c", body.Command)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			f.t.Error("fixture shell failed")
			w.WriteHeader(500)
			return
		}
		f.commandStdout, f.commandStderr = string(out), stderr.String()
		for _, name := range []string{"", "stdout", "stderr"} {
			info, err := os.Stat(filepath.Join(f.directory, name))
			if err != nil {
				f.t.Error(err)
				continue
			}
			want := os.FileMode(0600)
			if name == "" {
				want = 0700
			}
			if info.Mode().Perm() != want {
				f.t.Errorf("private output permissions %o, want %o", info.Mode().Perm(), want)
			}
		}
		res := map[string]any{"ok": true, "stdout": string(out), "stderr": stderr.String(), "exitCode": 0}
		if f.alterCommand != nil {
			f.alterCommand(res)
		}
		respond(f.t, w, res)
		return
	}
	if r.URL.Path != "/sandboxes/bx_test/files" {
		f.t.Errorf("unexpected fixture path %s", r.URL.Path)
		w.WriteHeader(404)
		return
	}
	name := r.URL.Query().Get("path")
	if f.directory == "" || (name != f.directory && name != f.directory+"/stdout" && name != f.directory+"/stderr") {
		f.t.Error("unexpected output filename")
		w.WriteHeader(400)
		return
	}
	if r.Method == "DELETE" {
		if r.URL.Query().Get("recursive") != "true" || name != f.directory {
			f.t.Error("cleanup did not target private directory")
			w.WriteHeader(400)
			return
		}
		if f.cleanupStatus != 0 {
			w.WriteHeader(f.cleanupStatus)
			respond(f.t, w, map[string]any{"ok": false, "message": "SECRET-CLEANUP"})
			return
		}
		if err := os.RemoveAll(f.directory); err != nil {
			f.t.Error(err)
		}
		f.deleted = true
		respond(f.t, w, map[string]any{"ok": true, "success": true})
		return
	}
	if r.Method != "GET" || r.URL.Query().Get("encoding") != "utf8" {
		f.t.Error("invalid file read")
		w.WriteHeader(400)
		return
	}
	data, err := os.ReadFile(name)
	if err != nil {
		f.t.Error(err)
		w.WriteHeader(404)
		return
	}
	res := map[string]any{"ok": true, "success": true, "path": name, "encoding": "utf8", "size": len(data), "content": string(data)}
	if f.alterRead != nil {
		f.alterRead(res)
	}
	if f.onRead != nil {
		f.onRead()
	}
	respond(f.t, w, res)
}

func (f *executionFixture) checkDeleted(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.deleted {
		t.Error("private output directory was not removed")
	}
	if _, err := os.Stat(f.directory); !errors.Is(err, os.ErrNotExist) {
		t.Error("private output files remain")
	}
}

func TestExecQuotesArgumentsAndEnvironment(t *testing.T) {
	c, f := fixtureClient(t)
	arg := "x'; printf INJECTED; #\n$(printf injected)"
	res, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"sh", "-c", `printf '%s|%s' "$VALUE" "$1"`, "test", arg}, Env: map[string]string{"VALUE": "it's literal; $(nope)"}})
	if err != nil || res.Stdout != "it's literal; $(nope)|"+arg || res.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", res, err)
	}
	if f.commandStdout != "0\n" || f.commandStderr != "" {
		t.Fatal("command history received output instead of status")
	}
	f.checkDeleted(t)
}

func TestExecKeepsSecretOutputsOutOfCommandHistory(t *testing.T) {
	c, f := fixtureClient(t)
	res, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"sh", "-c", `printf 'TOKENMARKER'; printf 'ERRORMARKER' >&2; exit 7`}})
	if err != nil || res.Stdout != "TOKENMARKER" || res.Stderr != "ERRORMARKER" || res.ExitCode != 7 {
		t.Fatalf("wrong output transport: exit=%d err=%v", res.ExitCode, err)
	}
	if f.commandStdout != "7\n" || f.commandStderr != "" {
		t.Fatal("sensitive process output reached command history")
	}
	f.checkDeleted(t)
}

func TestExecAcceptsBoatWorkdirRelativeOutputPaths(t *testing.T) {
	c, f := fixtureClient(t)
	// Captured live 2026-11-18: GET /files?path=/tmp/<marker>&encoding=utf8
	// returned {success:true,path:"../../tmp/<marker>",encoding:"utf8",
	// size:15,content:<15 UTF-8 bytes>}. Content was not printed or recorded.
	f.alterRead = func(m map[string]any) {
		m["path"] = "../../" + strings.TrimPrefix(m["path"].(string), "/")
	}
	res, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"printf", "t3fleet-marker\n"}})
	if err != nil || res.Stdout != "t3fleet-marker\n" || res.Stderr != "" || res.ExitCode != 0 {
		t.Fatalf("relative Boat file response rejected: exit=%d err=%v", res.ExitCode, err)
	}
	f.checkDeleted(t)
}

func TestExecRejectsInvalidInput(t *testing.T) {
	c := clientFor(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached API") })
	for _, req := range []provider.ExecRequest{{}, {Args: []string{"x\x00y"}}, {Args: []string{"true"}, Env: map[string]string{"BAD=KEY": "value"}}} {
		if _, err := c.Exec(t.Context(), "bx_test", req); err == nil {
			t.Error("accepted invalid exec input")
		}
	}
}

func TestExecRejectsMalformedCompletionAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]any)
	}{
		{"secret-status", func(m map[string]any) { m["stdout"] = "TOKENMARKER" }},
		{"out-of-range", func(m map[string]any) { m["stdout"] = "999\n" }},
		{"negative", func(m map[string]any) { m["stdout"] = "-1\n" }},
		{"stderr", func(m map[string]any) { m["stderr"] = "TOKENMARKER" }},
		// Observed on a cold snapshot restore: the provider's login profile
		// emitted this before our script could redirect stderr. Fix the image's
		// locale initialization; do not weaken the private output guarantee.
		{"profile-locale-warning", func(m map[string]any) {
			m["stderr"] = "/etc/profile.d/dev-environment.sh: line 2: warning: setlocale: LC_ALL: cannot change locale (en_US.UTF-8)\n"
		}},
		{"missing-exit", func(m map[string]any) { delete(m, "exitCode") }},
		{"outer-failure", func(m map[string]any) { m["exitCode"] = 125 }},
		{"truncated", func(m map[string]any) { m["stdoutTruncated"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fixtureClient(t)
			f.alterCommand = tc.alter
			_, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"printf", "TOKENMARKER"}})
			if err == nil || strings.Contains(err.Error(), "TOKENMARKER") {
				t.Fatalf("unsafe completion error: %v", err)
			}
			if tc.name == "profile-locale-warning" {
				typed, ok := errors.AsType[*ExecError](err)
				if !ok || !strings.Contains(typed.Error(), "unexpected_api_stderr") || strings.Contains(typed.Error(), "en_US") {
					t.Fatalf("profile warning was not classified safely: %v", err)
				}
			}
			f.checkDeleted(t)
		})
	}
}

func TestExecRejectsMalformedFileResponseAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]any)
	}{
		{"encoding", func(m map[string]any) { m["encoding"] = "base64" }},
		{"missing-size", func(m map[string]any) { delete(m, "size") }},
		{"missing-content", func(m map[string]any) { delete(m, "content") }},
		{"negative-size", func(m map[string]any) { m["size"] = -1 }},
		{"oversize", func(m map[string]any) { m["size"] = maxExecOutput + 1 }},
		{"mismatch-size", func(m map[string]any) { m["size"] = 0 }},
		{"path", func(m map[string]any) { m["path"] = "/etc/passwd" }},
		{"unrelated-relative-path", func(m map[string]any) { m["path"] = "../../tmp/unrelated-file" }},
		{"wrong-private-stream", func(m map[string]any) {
			m["path"] = "../../" + strings.TrimPrefix(path.Dir(m["path"].(string))+"/different-output", "/")
		}},
		{"failure", func(m map[string]any) { m["success"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := fixtureClient(t)
			f.alterRead = tc.alter
			_, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"printf", "TOKENMARKER"}})
			if err == nil || strings.Contains(err.Error(), "TOKENMARKER") {
				t.Fatalf("unsafe file error: %v", err)
			}
			f.checkDeleted(t)
		})
	}
}

func TestExecCleanupUsesIndependentContextAfterCancellation(t *testing.T) {
	c, f := fixtureClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.onRead = cancel
	_, err := c.Exec(ctx, "bx_test", provider.ExecRequest{Args: []string{"printf", "TOKENMARKER"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	f.checkDeleted(t)
}

func TestExecCleanupFailureDiscardsRetrievedSecrets(t *testing.T) {
	c, f := fixtureClient(t)
	f.cleanupStatus = http.StatusForbidden
	res, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"printf", "TOKENMARKER"}})
	if err == nil || res.Stdout != "" || res.Stderr != "" || strings.Contains(err.Error(), "TOKENMARKER") || strings.Contains(err.Error(), "SECRET-CLEANUP") {
		t.Fatalf("unsafe cleanup handling: %v", err)
	}
}

func TestExecCleanupAfterCommandAPIFailure(t *testing.T) {
	c, f := fixtureClient(t)
	f.commandStatus = http.StatusBadGateway
	_, err := c.Exec(t.Context(), "bx_test", provider.ExecRequest{Args: []string{"true"}})
	if err == nil {
		t.Fatal("accepted command failure")
	}
	f.checkDeleted(t)
}

func TestConnectBootstrapsPrivateCookieWithoutLeakingAuthorization(t *testing.T) {
	requests := 0
	grant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Boat-Org") != "" {
			t.Error("account authorization reached hosted port")
		}
		if r.URL.Path != "/.well-known/t3/environment" {
			t.Error("bootstrap did not request public T3 descriptor")
		}
		if r.URL.Query().Get("_token") == "port-secret" {
			http.SetCookie(w, &http.Cookie{Name: "boat-port", Value: "grant-secret", Path: "/", Secure: true, HttpOnly: true})
			http.Redirect(w, r, "/.well-known/t3/environment", http.StatusFound)
			return
		}
		cookie, err := r.Cookie("boat-port")
		if err != nil || cookie.Value != "grant-secret" {
			t.Error("private grant cookie was not sent on same-origin redirect")
		}
		respond(t, w, map[string]any{"environmentId": "env-test"})
	}))
	t.Cleanup(grant.Close)
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		if r.URL.Path != "/sandboxes/bx_test/host" || body["port"] != float64(3773) || body["public"] != false || body["localhost"] != true {
			t.Errorf("invalid host request: %s", data)
		}
		respond(t, w, map[string]any{"ok": true, "url": grant.URL + "/?_token=port-secret"})
	})
	c.accessHTTP = grant.Client()
	ep, err := c.Connect(t.Context(), "bx_test", 3773)
	if err != nil || strings.Contains(ep.URL, "_token") || ep.Headers.Get("Cookie") != "boat-port=grant-secret" || ep.Headers.Get("Authorization") != "" || requests != 2 {
		t.Fatalf("connect failed: %v", err)
	}
}

func TestConnectRefusesCrossOriginGrantRedirect(t *testing.T) {
	external := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("private endpoint followed external redirect")
	}))
	t.Cleanup(external.Close)
	grant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "boat-port", Value: "grant-secret", Path: "/", Secure: true})
		http.Redirect(w, r, external.URL+"/?_token=port-secret", http.StatusFound)
	}))
	t.Cleanup(grant.Close)
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "url": grant.URL + "/?_token=port-secret"})
	})
	c.accessHTTP = grant.Client()
	_, err := c.Connect(t.Context(), "bx_test", 3773)
	if err == nil || strings.Contains(err.Error(), "port-secret") || strings.Contains(err.Error(), "grant-secret") || strings.Contains(err.Error(), external.URL) {
		t.Fatalf("unsafe redirect result: %v", err)
	}
}

func TestConnectRejectsMalformedPrivateGrants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{name: "missing-cookie", status: 200},
		{name: "expired-cookie", cookie: &http.Cookie{Name: "grant", Value: "secret", Path: "/", MaxAge: -1}, status: 200},
		{name: "descriptor-only-cookie", cookie: &http.Cookie{Name: "grant", Value: "secret", Path: "/.well-known/t3"}, status: 200},
		{name: "wrong-domain", cookie: &http.Cookie{Name: "grant", Value: "secret", Path: "/", Domain: "example.com"}, status: 200},
		{name: "access-denied", cookie: &http.Cookie{Name: "grant", Value: "secret", Path: "/"}, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.cookie != nil {
					http.SetCookie(w, tc.cookie)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(grant.Close)
			c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
				respond(t, w, map[string]any{"ok": true, "url": grant.URL + "/?_token=port-secret"})
			})
			c.accessHTTP = grant.Client()
			if _, err := c.Connect(t.Context(), "bx_test", 3773); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("accepted malformed private access: %v", err)
			}
		})
	}
}

func TestConnectBoundsGrantRedirects(t *testing.T) {
	requests := 0
	grant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.SetCookie(w, &http.Cookie{Name: "grant", Value: "secret", Path: "/", Secure: true})
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	t.Cleanup(grant.Close)
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "url": grant.URL + "/?_token=port-secret"})
	})
	c.accessHTTP = grant.Client()
	if _, err := c.Connect(t.Context(), "bx_test", 3773); err == nil || requests != 6 {
		t.Fatalf("unbounded redirects: requests=%d err=%v", requests, err)
	}
}

func TestConnectGrantHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	grant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(grant.Close)
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, map[string]any{"ok": true, "url": grant.URL + "/?_token=port-secret"})
	})
	c.accessHTTP = grant.Client()
	if _, err := c.Connect(ctx, "bx_test", 3773); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant ignored cancellation: %v", err)
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

func TestCreateFallbackRequiresConfirmedRefusal(t *testing.T) {
	for _, tt := range []struct {
		name, method, code string
		status             int
		rejected           bool
	}{
		{"billing", "POST", "payment_required", 402, true},
		{"limit", "POST", "limit_reached", 429, true},
		{"unclassified billing", "POST", "", 402, false},
		{"unclassified limit", "POST", "", 429, false},
		{"capacity", "POST", "out_of_capacity", 503, true},
		{"unknown 503", "POST", "unavailable", 503, false},
		{"unknown 500", "POST", "internal_error", 500, false},
		{"lookup", "GET", "limit_reached", 429, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == tt.method {
					w.WriteHeader(tt.status)
					respond(t, w, map[string]any{"ok": false, "code": tt.code})
					return
				}
				respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}})
			})
			m, err := c.Create(t.Context(), provider.CreateRequest{Template: provider.Template{Snapshot: "golden"}, OperationID: "op"})
			if m.ID != "" || err == nil || errors.Is(err, provider.ErrCreateRejected) != tt.rejected {
				t.Fatalf("unexpected refusal classification: %v, %v", m, err)
			}
		})
	}
}

func TestCreateDoesNotReplayLostPostBeforeReconciliation(t *testing.T) {
	var posts int
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}})
			return
		}
		posts++
		if posts != 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			respond(t, w, map[string]any{"ok": false, "code": "limit_reached"})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	_, err := c.Create(t.Context(), provider.CreateRequest{Template: provider.Template{Snapshot: "golden"}, OperationID: "op"})
	if err == nil || errors.Is(err, provider.ErrCreateRejected) {
		t.Fatalf("lost response was treated as an explicit rejection: %v", err)
	}
}
