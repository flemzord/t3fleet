package fleet_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

// TestCloudLifecycle is explicitly opt-in: it creates and deletes billable
// resources using the selected account. Never enable it in pull-request CI.
// State stays in the configured private directory if cleanup is interrupted.
func TestCloudLifecycle(t *testing.T) {
	path := os.Getenv("T3FLEET_LIVE_CONFIG")
	if path == "" {
		t.Skip("set T3FLEET_LIVE_CONFIG and T3FLEET_LIVE_PROVIDERS for authorized cloud trials")
	}
	selection := os.Getenv("T3FLEET_LIVE_PROVIDERS")
	if selection == "" {
		t.Fatal("explicit T3FLEET_LIVE_PROVIDERS selection is required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := cfg.BuildProviders()
	if err != nil {
		t.Fatal(err)
	}
	for name := range strings.SplitSeq(selection, ",") {
		t.Run(name, func(t *testing.T) {
			p := providers[name]
			if p == nil {
				t.Fatal("selected provider is not configured")
			}
			if cfg.Templates[name] == (provider.Template{}) {
				t.Fatal("selected provider has no prepared template")
			}
			disk, err := state.Open(filepath.Join(cfg.StateDir, "canary-"+name))
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			m, err := fleet.New(fleet.Options{Providers: providers, Templates: cfg.Templates, Store: disk, ExpectedVersion: cfg.ExpectedVersion, ReadyTimeout: 5 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_ = m.Close(ctx)
			}()
			if len(m.List()) != 0 {
				t.Fatal("prior canary state is nonempty; reconcile its remote machine before starting another")
			}
			var nonce [3]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			machineName := "t3fleet-canary-" + name + "-" + hex.EncodeToString(nonce[:])
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
			defer cancel()
			machine, createErr := m.Create(ctx, fleet.CreateRequest{Name: machineName, Provider: name})
			// Install cleanup before checking readiness: an allocated but failed boot
			// must not turn a test failure into an untracked running machine.
			if machine.ID != "" {
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer cancel()
					if err := m.Delete(cleanup, machine.ID); err != nil {
						t.Errorf("cloud cleanup failed; local machine %s remains in private canary state: %v", machine.ID, err)
					}
				}()
			}
			if createErr != nil {
				t.Fatalf("cloud create/readiness failed (saved machine %s): %v", machine.ID, createErr)
			}
			t.Logf("%s ready: remote %s, local %s", name, machine.RemoteID, machine.ID)
			link, err := m.Pair(ctx, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			pairURL, err := url.Parse(link.URL)
			if err != nil {
				t.Fatal("invalid local pairing URL")
			}
			fragment, _ := url.ParseQuery(pairURL.Fragment)
			response, err := http.PostForm(machine.URL+"/oauth/token", url.Values{"subject_token": {fragment.Get("token")}})
			if err != nil {
				t.Fatal("pair exchange transport failed")
			}
			var exchange struct {
				Token string `json:"access_token"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&exchange)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil || exchange.Token == "" {
				t.Fatal("pair exchange failed")
			}
			cloudRPC(t, ctx, machine.URL, exchange.Token)
			// The real server's shell endpoint checks protocol version as well as auth.
			req, _ := http.NewRequestWithContext(ctx, "GET", machine.URL+"/api/orchestration/shell?omitIdleSubagents=1&omitOlderSettled=1", nil)
			req.Header.Set("Authorization", "Bearer "+exchange.Token)
			req.Header.Set("X-T3-Orchestration-Protocol", "2")
			shell, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal("cloud orchestration transport failed")
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(shell.Body, 4<<20))
			shell.Body.Close()
			if shell.StatusCode != http.StatusOK {
				t.Fatalf("cloud orchestration HTTP%d", shell.StatusCode)
			}
			if err := m.Stop(ctx, machine.ID); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Minute)
			for {
				remote, err := p.Get(ctx, machine.RemoteID)
				if err != nil {
					t.Fatal("cannot verify cloud stop")
				}
				if remote.State == provider.StateStopped {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("cloud stop not observed")
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(250 * time.Millisecond):
				}
			}
			descriptor, err := http.Get(machine.URL + "/.well-known/t3/environment")
			if err != nil {
				t.Fatal("cached descriptor failed")
			}
			descriptor.Body.Close()
			remote, err := p.Get(ctx, machine.RemoteID)
			if descriptor.StatusCode != http.StatusOK || err != nil || remote.State != provider.StateStopped {
				t.Fatal("public descriptor woke the stopped machine")
			}
			if err := m.Start(ctx, machine.ID); err != nil {
				t.Fatal(err)
			}
			cloudRPC(t, ctx, machine.URL, exchange.Token)
			t.Logf("%s passed: prepared template, readiness, pairing, HTTP, WebSocket, stop, no-wake descriptor, resume", name)
		})
	}
}

func cloudRPC(t *testing.T, ctx context.Context, origin, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", origin+"/api/auth/websocket-ticket/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("ticket request failed")
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&ticket)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || ticket.Ticket == "" {
		t.Fatal("ticket issuance failed")
	}
	wsURL := "ws" + strings.TrimPrefix(origin, "http") + "/ws?wsTicket=" + url.QueryEscape(ticket.Ticket)
	socket, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal("cloud WebSocket connection failed")
	}
	defer socket.CloseNow()
	socket.SetReadLimit(16 << 20)
	request := []byte(`{"_tag":"Request","id":"cloud-config","tag":"server.getConfig","payload":{},"headers":[]}`)
	if err := socket.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal("cloud RPC send failed")
	}
	for {
		_, data, err := socket.Read(ctx)
		if err != nil {
			t.Fatal("cloud RPC response failed")
		}
		var frames []map[string]any
		if len(data) > 0 && data[0] == '[' {
			if json.Unmarshal(data, &frames) != nil {
				t.Fatal("invalid cloud RPC frames")
			}
		} else {
			var frame map[string]any
			if json.Unmarshal(data, &frame) != nil {
				continue
			}
			frames = []map[string]any{frame}
		}
		for _, frame := range frames {
			if frame["_tag"] != "Exit" || frame["requestId"] != "cloud-config" {
				continue
			}
			exit, _ := frame["exit"].(map[string]any)
			if exit["_tag"] != "Success" {
				t.Fatal("cloud config RPC failed")
			}
			value, _ := exit["value"].(map[string]any)
			if _, bypass := value["directEndpoints"]; bypass {
				t.Fatal("cloud RPC bypasses proxy")
			}
			return
		}
	}
}
