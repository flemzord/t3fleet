package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
)

func TestCreateDesktopHandoff(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noRegister   bool
		createStatus int
		pairStatus   int
		pairURL      string
		launchFails  bool
		wantError    string
		wantCalls    []string
	}{
		{name: "automatic", wantCalls: []string{"create", "pair", "open"}},
		{name: "headless", noRegister: true, wantCalls: []string{"create"}},
		{name: "create-failed", createStatus: 500, wantError: "operation_failed", wantCalls: []string{"create"}},
		{name: "pair-failed", pairStatus: 500, wantError: "association could not be prepared", wantCalls: []string{"create", "pair"}},
		{name: "bad-link", pairURL: "https://attacker.example/pair#token=secret", wantError: "pairing response was invalid", wantCalls: []string{"create", "pair"}},
		{name: "launcher-failed", launchFails: true, wantError: "T3 Code could not be opened", wantCalls: []string{"create", "pair", "open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const credential = "one-use-private-credential"
			cfg := config.Default()
			cfg.StateDir = t.TempDir()
			token, err := ensureAdminToken(cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			record := func(call string) { mu.Lock(); defer mu.Unlock(); calls = append(calls, call) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("handoff did not use the authenticated admin API")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/v1/machines":
					record("create")
					if tc.createStatus != 0 {
						w.WriteHeader(tc.createStatus)
						_, _ = io.WriteString(w, `{"error":"operation_failed"}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"machine-1","state":"ready"}`)
				case "/v1/machines/machine-1/pair":
					record("pair")
					if tc.pairStatus != 0 {
						w.WriteHeader(tc.pairStatus)
						_, _ = io.WriteString(w, `{"error":"operation_failed"}`)
						return
					}
					link := tc.pairURL
					if link == "" {
						link = "http://127.0.0.1:47800/pair#token=" + credential
					}
					data, _ := json.Marshal(map[string]string{"url": link})
					_, _ = w.Write(data)
				default:
					t.Error("unexpected handoff endpoint")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg.Listen = strings.TrimPrefix(server.URL, "http://")
			previous := openT3
			t.Cleanup(func() { openT3 = previous })
			openT3 = func(_ context.Context, link string) error {
				record("open")
				u, err := url.Parse(link)
				if err != nil || u.Scheme != "t3code" || u.Host != "pair" || u.Query().Get("pairingUrl") != "http://127.0.0.1:47800/pair#token="+credential {
					t.Error("T3 did not receive the expected pairing link")
				}
				if tc.launchFails {
					return errors.New("launcher failed for " + link)
				}
				return nil
			}
			args := []string{"--name", "demo", "--provider", "boat", "--json"}
			if tc.noRegister {
				args = append(args, "--no-register")
			}
			var out, diagnostics bytes.Buffer
			err = runClient(t.Context(), cfg, "create", args, &out, &diagnostics)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("unexpected handoff result: %v", err)
			}
			mu.Lock()
			gotCalls := slices.Clone(calls)
			mu.Unlock()
			if !slices.Equal(gotCalls, tc.wantCalls) {
				t.Fatalf("handoff calls: %v, want %v", gotCalls, tc.wantCalls)
			}
			visible := out.String() + diagnostics.String()
			if err != nil {
				visible += err.Error()
			}
			if strings.Contains(visible, credential) || strings.Contains(visible, "token=") {
				t.Fatal("automatic handoff exposed the one-use credential")
			}
			if tc.createStatus == 0 {
				var machine struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(out.Bytes(), &machine) != nil || machine.ID != "machine-1" {
					t.Fatal("created identity was not preserved as JSON, including on handoff failure")
				}
				if tc.wantError != "" && (!strings.Contains(err.Error(), "machine-1") || !strings.Contains(err.Error(), "t3fleet pair")) {
					t.Fatal("handoff failure did not explain recovery without another create")
				}
			}
			if tc.name == "automatic" && !strings.Contains(diagnostics.String(), "Confirm") {
				t.Fatal("launcher success incorrectly implied association was finished")
			}
		})
	}
}

func TestPairingAppLinkRejectsInvalidTargets(t *testing.T) {
	for _, link := range []string{
		"http://127.0.0.1:47800@attacker.example/pair#token=secret",
		"http://localhost:47800.evil/pair#token=secret",
		"http://127.0.0.1:65536/pair#token=secret",
		"http://127.0.0.1:47800/other#token=secret",
		"http://127.0.0.1:47800/pair",
		"http://127.0.0.1:47800/pair?token=secret",
	} {
		data, _ := json.Marshal(map[string]string{"url": link})
		if _, err := pairingAppLink(data); err == nil {
			t.Error("invalid association target was accepted")
		}
	}
}

func TestDesktopLaunchPassesOneURLArgumentWithoutShell(t *testing.T) {
	const link = "t3code://pair?pairingUrl=http%3A%2F%2F127.0.0.1%3A1%2Fpair%23token%3Dprivate"
	for _, platform := range []string{"darwin", "linux", "windows"} {
		program, args, err := desktopCommand(platform, link)
		if err != nil || program == "" || args[len(args)-1] != link || slices.Contains(args, "-c") {
			t.Fatalf("incorrect launcher for %s", platform)
		}
	}
	if _, _, err := desktopCommand("unsupported", link); err == nil {
		t.Fatal("unsupported platform was not reported")
	}
}
