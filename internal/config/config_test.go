package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndStrictJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := Load(writeConfig(t, `{"providers":{"boat":{"apiKeyEnv":"MY_BOAT_TOKEN"}},"templates":{"boat":{"snapshot":"golden"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:47730" || cfg.ExpectedVersion != DefaultVersion || cfg.Templates["boat"].Snapshot != "golden" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	for _, input := range []string{`{"secret":"never-print-this"}`, `{"providers":{"boat":{"apiKey":"never-print-this"}}}`, `{} {}`, `{"listen":`, `null`, `[]`} {
		_, err := Load(writeConfig(t, input))
		if err == nil {
			t.Fatalf("accepted invalid config %q", input)
		}
		if strings.Contains(err.Error(), "never-print-this") {
			t.Fatal("configuration error leaked input")
		}
	}
}

func TestValidateLoopbackAndTemplates(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{"public-listen", func(c *Config) { c.Listen = "0.0.0.0:47730" }},
		{"dns-listen", func(c *Config) { c.Listen = "localhost:47730" }},
		{"missing-port", func(c *Config) { c.Listen = "127.0.0.1" }},
		{"public-machine-host", func(c *Config) { c.MachineHost = "0.0.0.0" }},
		{"relative-state", func(c *Config) { c.StateDir = "state" }},
		{"root-state", func(c *Config) { c.StateDir = "/" }},
		{"missing-version", func(c *Config) { c.ExpectedVersion = "" }},
		{"unknown-provider", func(c *Config) { c.Templates["other"] = c.Templates["boat"] }},
		{"conflicting-template", func(c *Config) {
			v := c.Templates["boat"]
			v.Image = "image"
			v.Snapshot = "snapshot"
			c.Templates["boat"] = v
		}},
		{"namespace-snapshot-without-source", func(c *Config) { v := c.Templates["boat"]; v.Snapshot = "snap"; c.Templates["namespace"] = v }},
		{"relative-key", func(c *Config) { c.Providers.Boat = &Boat{APIKeyFile: "key"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.modify(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestDispatchOnDemandConfiguration(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.DispatchPOC = &DispatchPOC{
			Listen:   "127.0.0.1:47731",
			OnDemand: &DispatchOnDemand{T3Binary: "/opt/t3/bin/t3"},
		}
		cfg.Providers.Boat = &Boat{}
		cfg.Templates["boat"] = provider.Template{Snapshot: "golden"}
		return cfg
	}
	cfg := base()
	if err := cfg.Validate(); err != nil || cfg.DispatchPOC.OnDemand.MaxWorkerCount() != 8 {
		t.Fatalf("zero-worker on-demand config rejected or wrong limit: %v", err)
	}
	cfg.DispatchPOC.OnDemand.NamespaceFallback = true
	cfg.Providers.Namespace = &Namespace{}
	cfg.Templates["namespace"] = provider.Template{Image: "registered:formance/max-dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Namespace fallback configuration rejected: %v", err)
	}
	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{"fallback without provider", func(c *Config) { c.DispatchPOC.OnDemand.NamespaceFallback = true }},
		{"fallback without image", func(c *Config) {
			c.DispatchPOC.OnDemand.NamespaceFallback = true
			c.Providers.Namespace = &Namespace{}
		}},
		{"mixed with fixed workers", func(c *Config) { c.DispatchPOC.Workers = []string{"a", "b"} }},
		{"no Boat config", func(c *Config) { c.Providers.Boat = nil }},
		{"no Boat template", func(c *Config) { delete(c.Templates, "boat") }},
		{"relative binary", func(c *Config) { c.DispatchPOC.OnDemand.T3Binary = "t3" }},
		{"oversized pool", func(c *Config) { c.DispatchPOC.OnDemand.MaxWorkers = 9 }},
		{"idle management", func(c *Config) { c.IdleTimeout = "2m" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.modify(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("incompatible on-demand configuration accepted")
			}
		})
	}
}

func TestBoatCredentialsPriorityAndRedaction(t *testing.T) {
	t.Setenv("BOAT_API_KEY", "from-env")
	key, org, err := (Boat{Organization: "wallet"}).credentials()
	if err != nil || key != "from-env" || org != "wallet" {
		t.Fatalf("env credentials: %q %q %v", key, org, err)
	}
	t.Setenv("BOAT_API_KEY", "")
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, _, err = (Boat{APIKeyFile: file}).credentials()
	if err != nil || key != "from-file" {
		t.Fatalf("file credentials: %q %v", key, err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = (Boat{APIKeyFile: file}).credentials()
	if err == nil || strings.Contains(err.Error(), "from-file") {
		t.Fatal("unsafe key file accepted or leaked")
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "key-link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err = (Boat{APIKeyFile: link}).credentials(); err == nil {
		t.Fatal("symlink key accepted")
	}
	saved := writeConfig(t, `{"token":"cli-session","org":"saved-wallet"}`)
	key, org, err = (Boat{CredentialFile: saved}).credentials()
	if err != nil || key != "cli-session" || org != "saved-wallet" {
		t.Fatalf("saved credentials: %q %q %v", key, org, err)
	}
	_, org, err = (Boat{CredentialFile: saved, Organization: "explicit-wallet"}).credentials()
	if err != nil || org != "explicit-wallet" {
		t.Fatal("explicit wallet not used")
	}
}

func TestBuildProvidersWithoutCredentialsOrSecrets(t *testing.T) {
	if _, err := Default().BuildProviders(); err == nil {
		t.Fatal("empty providers accepted")
	}
	t.Setenv("BOAT_API_KEY", "test-key")
	c := Default()
	c.Providers.Boat = &Boat{}
	ps, err := c.BuildProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps["boat"].Name() != "boat" {
		t.Fatal("Boat missing")
	}
	c.Providers.Boat.BaseURL = "https://bad.example/?secret=test-key"
	_, err = c.BuildProviders()
	if err == nil || strings.Contains(err.Error(), "test-key") {
		t.Fatal("bad endpoint accepted or leaked")
	}
}

func TestIdleTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
		want  time.Duration
	}{
		{"", true, 0}, {"0", true, 0}, {"0s", true, 0}, {"30m", true, 30 * time.Minute}, {"1m", true, time.Minute},
		{"-1m", false, 0}, {"59s", false, 0}, {"forever", false, 0}, {"999999999999999999h", false, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := Default()
			cfg.IdleTimeout = tc.value
			d, err := cfg.IdleDuration()
			if (err == nil) != tc.valid || err == nil && d != tc.want {
				t.Fatal(d, err)
			}
			if (cfg.Validate() == nil) != tc.valid {
				t.Fatal("validation disagrees")
			}
		})
	}
	cfg, err := Load(writeConfig(t, `{"idleTimeout":"30m"}`))
	if err != nil || cfg.IdleTimeout != "30m" {
		t.Fatal(cfg, err)
	}
	if _, err := Load(writeConfig(t, `{"idleTimeout":1800}`)); err == nil {
		t.Fatal("accepted numeric duration")
	}
}
