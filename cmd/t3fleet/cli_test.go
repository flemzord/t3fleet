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
	// This daemon ignores observe=1. Its fleet state is not a provider reading.
	if fields := strings.Fields(strings.Split(table, "\n")[1]); len(fields) < 5 || fields[3] != "-" || fields[4] != "-" {
		t.Fatalf("invented observation for an older daemon:\n%s", table)
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
	for _, args := range [][]string{{"admin", "stop", "agent"}, {"admin", "start", "3f9d"}, {"admin", "delete", "t3fleet-worker"}} {
		if err := runClient(t.Context(), cfg, args[0], args[1:], io.Discard, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{"POST /v1/machines/" + agentID + "/stop", "POST /v1/machines/" + workerID + "/start", "DELETE /v1/machines/" + workerID}
	if strings.Join(*calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	*calls = nil
	if err := runClient(t.Context(), cfg, "admin", []string{"delete", "3f9c"}, io.Discard, io.Discard); err == nil || len(*calls) != 0 {
		t.Fatalf("delete accepted an ID prefix: %v %v", err, *calls)
	}
}

func TestInventoryObservations(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	token := mustAdminToken(t, cfg.StateDir)
	const timestamp = "2026-11-19T21:20:10Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/_info" {
			_, _ = io.WriteString(w, `{"version":"test"}`)
			return
		}
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/machines?observe=1" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected inventory request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `[
			{"name":"one","futureField":true,"provider":"east","state":"ready","providerState":"stopped","providerObservedAt":"`+timestamp+`"},
			{"name":"two","provider":"east","state":"ready","providerState":"idle","providerObservedAt":"`+timestamp+`"},
			{"name":"three","provider":"west","state":"ready","providerState":"unknown"},
			{"name":"four","provider":"devboxes","state":"ready","providerState":"running","providerObservedAt":"`+timestamp+`"}]`)
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	run := func(cmd string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runClient(t.Context(), cfg, cmd, args, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	table := run("list")
	for _, want := range []string{"FLEET STATE", "PROVIDER STATE", "OBSERVED AT", timestamp, "stopped", "idle", "unknown"} {
		if !strings.Contains(table, want) {
			t.Errorf("list misses %q: %s", want, table)
		}
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(run("list", "--json")), &rows); err != nil || len(rows) != 4 {
		t.Fatalf("could not read list: %v, %v", rows, err)
	}
	if rows[0]["futureField"] != true || rows[0]["state"] != "ready" || rows[0]["providerState"] != "stopped" || rows[0]["providerObservedAt"] != timestamp ||
		rows[2]["providerState"] != "unknown" || rows[2]["providerObservedAt"] != nil || rows[3]["providerState"] != "running" {
		t.Fatalf("incorrect inventory observations: %v", rows)
	}
	status := run("status")
	for _, want := range []string{"4 (4 ready)", "Provider state (east):", "2 (1 idle, 1 stopped); last observed " + timestamp, "Provider state (west):", "1 (1 unknown)", "Provider state (devboxes):", "1 (1 running)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status misses %q: %s", want, status)
		}
	}
	var report statusReport
	if err := json.Unmarshal([]byte(run("status", "--json")), &report); err != nil {
		t.Fatal(err)
	}
	east, west := report.ProviderStates["east"], report.ProviderStates["west"]
	devboxes := report.ProviderStates["devboxes"]
	if report.Total != 4 || report.Machines["ready"] != 4 || len(report.ProviderStates) != 3 ||
		devboxes == nil || devboxes.Total != 1 || devboxes.States["running"] != 1 ||
		east == nil || east.Total != 2 || east.States["idle"] != 1 || east.States["stopped"] != 1 || east.ObservedAt.Format(time.RFC3339) != timestamp ||
		west == nil || west.Total != 1 || west.States["unknown"] != 1 || !west.ObservedAt.IsZero() {
		t.Fatalf("incorrect status: %+v", report)
	}
}

func TestStatusReportsUnavailableInventory(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mustAdminToken(t, cfg.StateDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_info" {
			_, _ = io.WriteString(w, `{"version":"test"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out bytes.Buffer
	err := runClient(t.Context(), cfg, "status", []string{"--json"}, &out, io.Discard)
	var report statusReport
	if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || !report.Daemon.Running || report.Machines != nil {
		t.Fatalf("unreadable inventory must not report success: %v %s", err, &out)
	}
}

func TestCreateUsesTheOnlyConfiguredProvider(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Providers["compute-west"] = config.ProviderConfig{Type: "namespace"}
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
	if err := runClient(t.Context(), cfg, "admin", []string{"create", "-n", "agent"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "compute-west" || !strings.Contains(out.String(), "Created t3fleet-agent") {
		t.Fatalf("provider=%q out=%q diag=%q", got.Provider, out.String(), diag.String())
	}
	cfg.Providers["primary"] = config.ProviderConfig{Type: "boat"}
	if err := runClient(t.Context(), cfg, "admin", []string{"create", "--name", "agent"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--provider") {
		t.Fatalf("ambiguous provider choice was not reported: %v", err)
	}
}

func TestStatusReportsDaemonAndMachines(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Providers["primary"] = config.ProviderConfig{Type: "boat"}
	cfg.Dispatch = &config.Dispatch{Listen: "127.0.0.1:47733", ProviderOrder: []string{"primary"}, MaxWorkers: 17}
	machineServer(t, &cfg)
	var out bytes.Buffer
	if err := runClient(t.Context(), cfg, "status", nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Providers:", "primary", "47733", "Worker limit:", "17", "running on", "2 (1 ready, 1 stopped)", "not reported"} {
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

func TestPairTargetsTheFleetWithZeroMachines(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	token := mustAdminToken(t, cfg.StateDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("expected authenticated POST")
		}
		switch r.URL.Path {
		case "/v1/dispatch/pair":
			_, _ = io.WriteString(w, `{"url":"http://127.0.0.1:47801/pair#token=one-time"}`)
		case "/v1/dispatch/pair-external":
			_, _ = io.WriteString(w, `{"url":"https://fleet.example/pair#token=one-time"}`)
		case "/v1/dispatch/pair-peer":
			var req struct{ Access string }
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Access != "approval-required" {
				t.Error("wrong access")
			}
			_, _ = io.WriteString(w, `{"credential":"test-code"}`)
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out bytes.Buffer
	if err := runClient(t.Context(), cfg, "pair", nil, &out, io.Discard); err != nil || !strings.HasPrefix(out.String(), "t3code://pair?pairingUrl=") {
		t.Fatalf("%v %q", err, out.String())
	}
	out.Reset()
	if err := runClient(t.Context(), cfg, "pair", []string{"--external"}, &out, io.Discard); err != nil ||
		!strings.Contains(out.String(), "https%3A%2F%2Ffleet.example%2Fpair") {
		t.Fatalf("external Desktop pairing failed: %v", err)
	}
	out.Reset()
	if err := runClient(t.Context(), cfg, "pair", []string{"--peer-access", "approval-required", "--json"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), `"credential": "test-code"`) {
		t.Fatalf("%v %q", err, out.String())
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
	err = runClient(t.Context(), cfg, "admin", []string{"start", "--bad"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "t3fleet admin start --help") {
		t.Fatalf("flag error has no help hint: %v", err)
	}
}

func TestHelpGroupsCommandsAndShowsExamples(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), []string{"--help"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fleet service:", "Worker inventory and recovery:", "init", "T3FLEET_CONFIG", "t3fleet pair"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("root help misses %q", want)
		}
	}
	for _, removed := range []string{"create", "delete", "open", "dispatch", "pair-dispatch"} {
		if strings.Contains(out.String(), "\n  "+removed+" ") {
			t.Errorf("%s exposed at root", removed)
		}
		if err := run(t.Context(), []string{removed}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s still callable at root: %v", removed, err)
		}
	}
	out.Reset()
	if err := run(t.Context(), []string{"admin", "create", "--help"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "Examples:") || !strings.Contains(out.String(), "--provider") || strings.Contains(out.String(), "--count") || strings.Contains(out.String(), "--snapshot") {
		t.Fatalf("create help incomplete: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run(t.Context(), []string{"--version"}, &out, io.Discard); err != nil || out.String() != "t3fleet dev\n" {
		t.Fatalf("--version: %v %q", err, out.String())
	}
}
