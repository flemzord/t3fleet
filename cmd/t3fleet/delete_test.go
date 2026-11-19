package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

func TestDeleteSelectionConfirmationAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, input          string
		args, want           []string
		interactive, wantErr bool
	}{
		{name: "confirmed", args: []string{"--ids", "three,one,three"}, input: "delete\n", interactive: true, want: []string{"three", "one"}},
		{name: "declined", args: []string{"--ids", "one,two"}, input: "no\n", interactive: true},
		{name: "eof at confirmation", args: []string{"--ids", "one,two"}, interactive: true},
		{name: "numeric input cannot bypass picker", input: "1 2\ndelete\n", interactive: true, wantErr: true},
		{name: "no implicit confirmation", args: []string{"--yes"}, interactive: true, wantErr: true},
		{name: "script", args: []string{"--ids", "two,one,two", "--yes"}, want: []string{"two", "one"}},
		{name: "unmanaged", args: []string{"--ids", "one,foreign", "--yes"}, wantErr: true},
		{name: "no-tty", wantErr: true},
		{name: "no-confirmation", args: []string{"--ids", "one"}, wantErr: true},
		{name: "no-all", args: []string{"--all", "--yes"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = t.TempDir()
			token := mustAdminToken(t, cfg.StateDir)
			var deleted []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("missing auth")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode([]fleet.Machine{{ID: "one", Name: "first", Provider: "mock", State: "ready"}, {ID: "two"}, {ID: "three"}})
					return
				}
				if r.Method != http.MethodDelete {
					t.Error("unexpected mutation")
				}
				deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/v1/machines/"))
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			cfg.Listen = strings.TrimPrefix(server.URL, "http://")
			var out, diag bytes.Buffer
			a := testApp(cfg, &out, &diag)
			a.stdin, a.stdinTTY = strings.NewReader(tc.input), tc.interactive
			err := a.execute(t.Context(), append([]string{"admin", "delete"}, tc.args...))
			if (err != nil) != tc.wantErr || !slices.Equal(deleted, tc.want) {
				t.Fatalf("err=%v deleted=%v want=%v", err, deleted, tc.want)
			}
		})
	}
}

func TestDeleteCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readSelection(ctx, bufio.NewScanner(strings.NewReader("1\n"))); err == nil {
		t.Fatal("cancelled selection accepted")
	}
}

func TestDeletePartialFailureContinuesOnlySelectedIDs(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mustAdminToken(t, cfg.StateDir)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]fleet.Machine{{ID: "one"}, {ID: "two"}, {ID: "untouched"}})
			return
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "one") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out, diag bytes.Buffer
	err := runClient(t.Context(), cfg, "admin", []string{"delete", "--ids", "one,two", "--yes", "--json"}, &out, &diag)
	if err == nil || !slices.Equal(calls, []string{"/v1/machines/one", "/v1/machines/two"}) {
		t.Fatal("wrong partial deletion", err, calls)
	}
	var results []struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}
	if json.Unmarshal(out.Bytes(), &results) != nil || len(results) != 2 || results[0].Deleted || !results[1].Deleted {
		t.Fatal("missing partial summary")
	}
}
