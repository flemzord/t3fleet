package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
)

func TestEnableControllerPeerLinksKeepsOtherSettings(t *testing.T) {
	userdata := t.TempDir()
	path := filepath.Join(userdata, "settings.json")
	if err := os.WriteFile(path, []byte(`{"linkedEnvironmentsEnabled":false,"newWorktreesStartFromOrigin":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enableControllerPeerLinks(userdata); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]bool
	if json.Unmarshal(data, &settings) != nil || len(settings) != 2 || !settings["linkedEnvironmentsEnabled"] || settings["newWorktreesStartFromOrigin"] {
		t.Fatal("controller Labs update did not preserve other settings")
	}
}

func TestLiveDispatchController(t *testing.T) {
	binary := os.Getenv("T3FLEET_T3_BINARY")
	if binary == "" {
		t.Skip("set T3FLEET_T3_BINARY to run the genuine local controller")
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.DispatchPOC = &config.DispatchPOC{OnDemand: &config.DispatchOnDemand{T3Binary: binary}}
	worker, stop, err := startDispatchController(t.Context(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	descriptor, err := worker.Descriptor(t.Context())
	if err != nil || descriptor["environmentId"] == "" {
		t.Fatal("local controller did not report its environment")
	}
	up, err := worker.Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unauth, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, up.Endpoint.URL+"/api/auth/session", nil)
	response, err := http.DefaultClient.Do(unauth)
	if err != nil {
		t.Fatal("could not contact local controller")
	}
	var session struct {
		Authenticated *bool `json:"authenticated"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&session)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || session.Authenticated == nil || *session.Authenticated {
		t.Fatal("controller did not correctly identify an unauthenticated local client")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "dispatch-controller", "runtime", "userdata")); err != nil {
		t.Fatal("controller did not create its own T3 userdata")
	}
	stop()
	worker, stop, err = startDispatchController(t.Context(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	reopened, err := worker.Descriptor(t.Context())
	if err != nil || reopened["environmentId"] != descriptor["environmentId"] {
		t.Fatal("controller lost its own T3 identity after restart")
	}
}
