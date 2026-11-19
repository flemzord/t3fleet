package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
)

func TestAdminOpenUsesExistingWorkerAndRedactsLauncherError(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	token := mustAdminToken(t, cfg.StateDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("not authenticated")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			_, _ = io.WriteString(w, `[{"id":"machine-1","name":"diagnostics"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/machine-1/pair":
			_, _ = io.WriteString(w, `{"url":"http://127.0.0.1:47800/pair#token=one-use-credential"}`)
		default:
			t.Error("unexpected machine mutation", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	previous := openT3
	t.Cleanup(func() { openT3 = previous })
	openT3 = func(_ context.Context, link string) error {
		if !strings.HasPrefix(link, "t3code://pair?pairingUrl=") || !strings.Contains(link, "one-use-credential") {
			t.Fatal("invalid handoff URL")
		}
		return errors.New("launcher failed: " + link)
	}
	var output bytes.Buffer
	err := runClient(t.Context(), cfg, "admin", []string{"open", "diagnostics"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "t3fleet admin pair") || strings.Contains(output.String()+err.Error(), "one-use-credential") {
		t.Fatalf("invalid launcher recovery: %v %s", err, output.String())
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
