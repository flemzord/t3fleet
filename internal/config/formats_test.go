package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTOMLAndJSONUseTheSameSchema(t *testing.T) {
	cfg, err := Load(writeConfig(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dispatch.Agents = []Agent{{Driver: "codex", Models: []string{"gpt-5.4"}}}
	cfg.Dispatch.MaxWorkers = 3
	path := filepath.Join(t.TempDir(), "config.toml")
	data, err := MarshalFile(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Fatal("TOML round trip changed the configuration")
	}
}

func TestTOMLRejectsUnknownDuplicateAndWrongTypeFields(t *testing.T) {
	const dispatch = `
[dispatch]
t3Binary = "/opt/t3"
listen = "127.0.0.1:47733"
providerOrder = ["boat"]
`
	const boat = `
[providers.boat]
type = "boat"
organization = "my-org"
template = { snapshot = "image" }
`
	for _, tc := range []struct {
		name, text string
	}{
		{"unknown key", "secret_field = 'never-print-this'\n" + dispatch + boat},
		{"wrong driver field even empty", dispatch + boat + "tenantId = ''\n"},
		{"unknown nested key", dispatch + strings.Replace(boat, `snapshot = "image"`, `snapshot = "image", unknown = "never-print-this"`, 1)},
		{"duplicate key", dispatch + boat + "organization = 'never-print-this'\n"},
		{"fractional number", dispatch + "maxWorkers = 2.0\n" + boat},
		{"unquoted TOML date", "expectedVersion = 2026-11-22\n" + dispatch + boat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || strings.Contains(err.Error(), "never-print-this") {
				t.Fatalf("expected a redacted validation error, got %v", err)
			}
		})
	}
}
