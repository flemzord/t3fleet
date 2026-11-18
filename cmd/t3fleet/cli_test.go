package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

const (
	agentID  = "3f9c1e2a00000000000000000000000a"
	workerID = "3f9d77aa00000000000000000000000b"
)

var sampleMachines = []fleet.Machine{
	{ID: agentID, Name: "t3fleet-agent", Provider: "boat", State: "ready"},
	{ID: workerID, Name: "t3fleet-worker", Provider: "namespace", State: "stopped", LastStopReason: "idle", LastStoppedAt: time.Now().Add(-2 * time.Hour)},
}

func TestResolveMachine(t *testing.T) {
	for _, tc := range []struct {
		ref, want   string
		allowPrefix bool
		wantErr     string
	}{
		{ref: agentID, want: agentID},
		{ref: "t3fleet-agent", want: agentID},
		{ref: "agent", want: agentID},
		{ref: "3f9c", want: agentID, allowPrefix: true},
		{ref: "3f9", allowPrefix: true, wantErr: "no managed machine"},
		{ref: "3f9c1", wantErr: "full ID or exact name"},
		{ref: "unknown", allowPrefix: true, wantErr: "no managed machine"},
		{ref: "../x", allowPrefix: true, wantErr: "invalid machine reference"},
	} {
		got, err := resolveMachine(sampleMachines, tc.ref, tc.allowPrefix)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: want error %q, got %v", tc.ref, tc.wantErr, err)
			}
			continue
		}
		if err != nil || got.ID != tc.want {
			t.Errorf("%q: got %q, %v", tc.ref, got.ID, err)
		}
	}
	if _, err := resolveMachine(sampleMachines, "3f9", true); err == nil {
		t.Error("prefix shorter than the minimum was accepted")
	}
	shared := []fleet.Machine{{ID: "abcd0001"}, {ID: "abcd0002"}}
	if _, err := resolveMachine(shared, "abcd", true); err == nil || !strings.Contains(err.Error(), "several machines") {
		t.Errorf("ambiguous prefix was not reported: %v", err)
	}
}

// machineServer serves sampleMachines and records mutations.
func machineServer(t *testing.T, cfg *config.Config) *[]string {
	t.Helper()
	token, err := ensureAdminToken(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_info" {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "t3fleet", "version": "dev"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/machines" {
			_ = json.NewEncoder(w).Encode(sampleMachines)
			return
		}
		if r.Method == http.MethodGet {
			for _, m := range sampleMachines {
				if r.URL.Path == "/v1/machines/"+m.ID {
					_ = json.NewEncoder(w).Encode(m)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	return calls
}

func TestListAndInspectHumanAndJSON(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	machineServer(t, &cfg)

	var out bytes.Buffer
	if err := runClient(t.Context(), cfg, "list", nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	table := out.String()
	for _, want := range []string{"NAME", "t3fleet-agent", "boat", "ready", "3f9c1e2a0000", "idle, 2h ago"} {
		if !strings.Contains(table, want) {
			t.Fatalf("list table misses %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "\x1b[") {
		t.Fatal("redirected list output contains color codes")
	}

	out.Reset()
	if err := runClient(t.Context(), cfg, "ls", []string{"--json"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var machines []fleet.Machine
	if json.Unmarshal(out.Bytes(), &machines) != nil || len(machines) != 2 {
		t.Fatalf("list --json is not the machine array: %s", out.String())
	}

	out.Reset()
	if err := runClient(t.Context(), cfg, "inspect", []string{"worker"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ID:") || !strings.Contains(out.String(), workerID) || !strings.Contains(out.String(), "namespace") {
		t.Fatalf("inspect details incomplete:\n%s", out.String())
	}
}

func TestMachineCommandsResolveReferences(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	calls := machineServer(t, &cfg)
	for _, args := range [][]string{{"stop", "agent"}, {"start", "3f9d"}, {"delete", "t3fleet-worker"}} {
		if err := runClient(t.Context(), cfg, args[0], args[1:], io.Discard, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{"POST /v1/machines/" + agentID + "/stop", "POST /v1/machines/" + workerID + "/start", "DELETE /v1/machines/" + workerID}
	if strings.Join(*calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	*calls = nil
	if err := runClient(t.Context(), cfg, "delete", []string{"3f9c"}, io.Discard, io.Discard); err == nil || len(*calls) != 0 {
		t.Fatalf("delete accepted an ID prefix: %v %v", err, *calls)
	}
}

func TestCreateUsesTheOnlyConfiguredProvider(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Providers.Namespace = &config.Namespace{}
	token := mustAdminToken(t, cfg.StateDir)
	var got fleet.CreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || json.NewDecoder(r.Body).Decode(&got) != nil {
			t.Error("unexpected create request")
		}
		_ = json.NewEncoder(w).Encode(fleet.Machine{ID: agentID, Name: "t3fleet-agent", Provider: got.Provider, State: "ready"})
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out, diag bytes.Buffer
	if err := runClient(t.Context(), cfg, "create", []string{"-n", "agent", "--no-register"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "namespace" || !strings.Contains(out.String(), "Created t3fleet-agent") || !strings.Contains(diag.String(), "t3fleet open t3fleet-agent") {
		t.Fatalf("provider=%q out=%q diag=%q", got.Provider, out.String(), diag.String())
	}
	cfg.Providers.Boat = &config.Boat{}
	if err := runClient(t.Context(), cfg, "create", []string{"--name", "agent"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--provider") {
		t.Fatalf("ambiguous provider choice was not reported: %v", err)
	}
}

func TestStatusReportsDaemonAndMachines(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Providers.Boat = &config.Boat{}
	machineServer(t, &cfg)
	var out bytes.Buffer
	if err := runClient(t.Context(), cfg, "status", nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Providers:", "boat", "running on", "2 (1 ready, 1 stopped)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status misses %q:\n%s", want, out.String())
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = listener.Addr().String()
	_ = listener.Close()
	out.Reset()
	err = runClient(t.Context(), cfg, "status", []string{"--json"}, &out, io.Discard)
	var report statusReport
	if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || report.Daemon.Running {
		t.Fatalf("stopped daemon not reported: %v %s", err, out.String())
	}
}

func TestDispatchPairAndDeprecatedAlias(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mustAdminToken(t, cfg.StateDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dispatch-poc/pair" {
			t.Error("unexpected path", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"url":"http://127.0.0.1:47801/pair#token=one-time"}`)
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	for _, args := range [][]string{{"dispatch", "pair"}, {"pair-dispatch"}} {
		var out bytes.Buffer
		if err := runClient(t.Context(), cfg, args[0], args[1:], &out, io.Discard); err != nil || !strings.HasPrefix(out.String(), "t3code://pair?pairingUrl=") {
			t.Fatalf("%v: %v %q", args, err, out.String())
		}
	}

	notEnabled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	defer notEnabled.Close()
	cfg.Listen = strings.TrimPrefix(notEnabled.URL, "http://")
	if err := runClient(t.Context(), cfg, "dispatch", []string{"pair"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "dispatch is not enabled") {
		t.Fatalf("disabled dispatch not explained: %v", err)
	}
}

func TestErrorsAreRenderedWithHints(t *testing.T) {
	err := run(t.Context(), []string{"lst"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("unknown command did not suggest list: %v", err)
	}
	var buf bytes.Buffer
	printError(&buf, err, false)
	if !strings.HasPrefix(buf.String(), "Error: unknown command") || !strings.Contains(buf.String(), "t3fleet --help") {
		t.Fatalf("unknown command rendering: %q", buf.String())
	}

	buf.Reset()
	printError(&buf, hinted("run 't3fleet list'", "no managed machine matches %q", "x"), false)
	if buf.String() != "Error: no managed machine matches \"x\"\nHint:  run 't3fleet list'\n" {
		t.Fatalf("hint rendering: %q", buf.String())
	}

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	err = runClient(t.Context(), cfg, "start", []string{"--bad"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "t3fleet start --help") {
		t.Fatalf("flag error has no help hint: %v", err)
	}
}

func TestHelpGroupsCommandsAndShowsExamples(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), []string{"--help"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Machine commands:", "T3 Code access:", "Setup and daemon:", "init", "T3FLEET_CONFIG"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("root help misses %q", want)
		}
	}
	if strings.Contains(out.String(), "pair-dispatch") {
		t.Fatal("deprecated alias is listed in help")
	}
	out.Reset()
	if err := run(t.Context(), []string{"create", "--help"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "Examples:") || !strings.Contains(out.String(), "--no-register") {
		t.Fatalf("create help incomplete: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run(t.Context(), []string{"--version"}, &out, io.Discard); err != nil || out.String() != "t3fleet dev\n" {
		t.Fatalf("--version: %v %q", err, out.String())
	}
}
