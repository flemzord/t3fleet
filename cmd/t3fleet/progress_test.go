package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestAnimatedProgressStopsBeforeFinalOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		finish := startProgress(&out, progressMessages{"Creating machine", "Machine ready", "Creation did not complete"}, true)
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		if strings.Count(out.String(), "Creating machine") < 2 {
			t.Fatal("waiting operation did not animate")
		}
		finish(nil)
		if !strings.HasSuffix(out.String(), "Machine ready (2s)\n") {
			t.Fatal("spinner was not replaced by the final status and elapsed time")
		}
		done := out.String()
		finish(errors.New("ignored second finish"))
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if out.String() != done {
			t.Fatal("renderer continued writing after completion")
		}
	})
}

func TestLogProgressHasNoTerminalEscapesOrErrorContents(t *testing.T) {
	var out bytes.Buffer
	finish := startProgress(&out, progressMessages{"Saving snapshot", "Snapshot ready", "Snapshot did not complete"}, false)
	finish(errors.New("private-provider-credential"))
	if !strings.HasPrefix(out.String(), "Saving snapshot…\n") || !strings.Contains(out.String(), "Snapshot did not complete") {
		t.Fatal("logs did not explain the pending and failed operation")
	}
	if strings.ContainsAny(out.String(), "\r\x1b") || strings.Contains(out.String(), "private-provider-credential") {
		t.Fatal("logs contain terminal controls or an arbitrary provider error")
	}
	if interactiveProgress(&out) {
		t.Fatal("redirected output must not animate")
	}
}

type progressBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *progressBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *progressBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestCreateShowsProgressWhileDaemonIsStillWorking(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Providers["boat"] = config.ProviderConfig{Type: "boat", Template: provider.Template{Snapshot: "golden"}}
	if _, err := ensureAdminToken(cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	requested, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		<-release
		_, _ = io.WriteString(w, `{"id":"machine-1","state":"ready"}`)
	}))
	defer server.Close()
	defer unblock()
	cfg.Listen = strings.TrimPrefix(server.URL, "http://")
	var out, diagnostics progressBuffer
	done := make(chan error, 1)
	go func() {
		done <- runClient(t.Context(), cfg, "admin", []string{"create", "--name", "demo", "--provider", "boat", "--json"}, &out, &diagnostics)
	}()
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("CLI did not call the daemon")
	}
	if !strings.Contains(diagnostics.String(), "machine on boat from snapshot") || strings.Contains(diagnostics.String(), "Machine ready") {
		t.Fatal("CLI did not show activity before the cloud operation finished")
	}
	if out.String() != "" {
		t.Fatal("progress polluted machine JSON on stdout")
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI did not finish after daemon completion")
	}
	if !strings.Contains(diagnostics.String(), "Machine ready") || !strings.Contains(out.String(), `"id": "machine-1"`) {
		t.Fatal("final machine result or completion status was lost")
	}
}
