package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
)

// isolateUserDirs points every default location at a temporary directory.
func isolateUserDirs(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("T3FLEET_CONFIG", "")
	return home
}

func initApp(out, diag io.Writer, input string, tty bool) *app {
	return &app{stdout: out, stderr: diag, stdin: strings.NewReader(input), stdinTTY: tty, load: config.Load}
}

func TestInitWritesPrivateValidConfigAndNeverOverwrites(t *testing.T) {
	home := isolateUserDirs(t)
	path := filepath.Join(home, "fresh", "t3fleet", "config.json")
	args := []string{"init", "--config", path, "--provider", "boat", "--boat-organization", "acme", "--boat-snapshot", "snap-1", "--no-input"}
	var out, diag bytes.Buffer
	if err := initApp(&out, &diag, "", true).execute(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Wrote "+path) || !strings.Contains(diag.String(), "T3FLEET_CONFIG="+path) || !strings.Contains(diag.String(), "BOAT_API_KEY") {
		t.Fatalf("missing confirmation or next steps:\n%s\n%s", out.String(), diag.String())
	}
	file, err := os.Stat(path)
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("config file is not private: %v", err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("config directory is not private: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Providers.Boat == nil || cfg.Providers.Boat.Organization != "acme" || cfg.Templates["boat"].Snapshot != "snap-1" || cfg.Providers.Namespace != nil {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	before, _ := os.ReadFile(path)
	err = initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"init", "--config", path, "--provider", "namespace"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing config was not protected: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("existing config changed")
	}
	if err := initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"init", "--config", path, "--provider", "namespace", "--force"}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := config.Load(path); err != nil || cfg.Providers.Boat != nil || cfg.Providers.Namespace == nil {
		t.Fatalf("--force did not replace the config: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestInitPromptsForMissingValues(t *testing.T) {
	home := isolateUserDirs(t)
	path := filepath.Join(home, "config.json")
	// providers, boat org, boat snapshot (skip), namespace site, source, snapshot
	input := "both\nacme\n\niad\ngolden\nsnap-9\n"
	var diag bytes.Buffer
	if err := initApp(io.Discard, &diag, input, true).execute(t.Context(), []string{"init", "--config", path}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Boat == nil || cfg.Providers.Boat.Organization != "acme" || cfg.Providers.Namespace == nil || cfg.Providers.Namespace.Site != "iad" {
		t.Fatalf("providers: %+v", cfg.Providers)
	}
	if _, ok := cfg.Templates["boat"]; ok || cfg.Templates["namespace"].Source != "golden" || cfg.Templates["namespace"].Snapshot != "snap-9" {
		t.Fatalf("templates: %+v", cfg.Templates)
	}
	if !strings.Contains(diag.String(), "Providers (boat, namespace, or both) [boat]:") || !strings.Contains(diag.String(), "templates.boat.snapshot") {
		t.Fatalf("prompts or next steps missing:\n%s", diag.String())
	}
}

func TestInitRejectsIncompleteInputAndPrintsWithoutWriting(t *testing.T) {
	home := isolateUserDirs(t)
	path := filepath.Join(home, "config.json")
	for _, args := range [][]string{
		{"init", "--config", path},
		{"init", "--config", path, "--provider", "aws"},
		{"init", "--config", path, "--provider", "namespace", "--namespace-snapshot", "snap"},
		{"init", "--config", path, "--provider", "boat", "--state-dir", "relative"},
	} {
		if err := initApp(io.Discard, io.Discard, "", false).execute(t.Context(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := initApp(&out, io.Discard, "", false).execute(t.Context(), []string{"init", "--config", path, "-p", "boat", "--print"}); err != nil {
		t.Fatal(err)
	}
	var printed config.Config
	if json.Unmarshal(out.Bytes(), &printed) != nil || printed.Providers.Boat == nil {
		t.Fatalf("--print output: %s", out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("init wrote a file despite errors or --print")
	}
}

func TestMissingDefaultConfigUsesDefaultsAndSuggestsInit(t *testing.T) {
	isolateUserDirs(t)
	err := initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"list"})
	if err == nil || !strings.Contains(err.Error(), "t3fleet init") {
		t.Fatalf("missing default config did not suggest init: %v", err)
	}
	var out bytes.Buffer
	err = initApp(&out, io.Discard, "", false).execute(t.Context(), []string{"status", "--json"})
	var report statusReport
	if err == nil || json.Unmarshal(out.Bytes(), &report) != nil || report.ConfigFound || report.Listen == "" {
		t.Fatalf("status without config: %v %s", err, out.String())
	}
	if err := initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"serve"}); err == nil || !strings.Contains(err.Error(), "t3fleet init") {
		t.Fatalf("serve without config: %v", err)
	}
	if _, err := os.Stat(config.DefaultPath()); !os.IsNotExist(err) {
		t.Fatal("a read command created the default configuration")
	}

	explicit := filepath.Join(t.TempDir(), "typo.json")
	err = initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"list", "--config", explicit})
	if err == nil || !strings.Contains(err.Error(), "cannot open configuration") {
		t.Fatalf("explicit missing config must stay an error: %v", err)
	}
	t.Setenv("T3FLEET_CONFIG", explicit)
	if err := initApp(io.Discard, io.Discard, "", false).execute(t.Context(), []string{"list"}); err == nil || !strings.Contains(err.Error(), "cannot open configuration") {
		t.Fatalf("missing T3FLEET_CONFIG must stay an error: %v", err)
	}
}
