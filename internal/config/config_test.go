package config

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

const fixture = `{"providers":{"fast":{"type":"boat","organization":"fast-org","template":{"snapshot":"golden"},"size":"large","apiKeyEnv":"FAST_TOKEN"},"burst":{"type":"boat","organization":"burst-org","template":{"snapshot":"burst-base"},"size":"xlarge","apiKeyEnv":"BURST_TOKEN"},"eu":{"type":"namespace","tenantId":"acme","template":{"image":"registered:acme/t3"},"virtualCPU":12,"memoryMB":32768,"architecture":"arm64","tokenFile":"/run/creds/namespace"}},"dispatch":{"listen":"127.0.0.1:47733","t3Binary":"/opt/t3","providerOrder":["burst","fast","eu"]}}`

func TestLoadNamedProviders(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := Load(writeConfig(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:47730" || cfg.ExpectedVersion != DefaultVersion || cfg.Providers["eu"].Type != "namespace" || cfg.Providers["eu"].VirtualCPU != 12 || cfg.Providers["eu"].TokenFile != "/run/creds/namespace" || cfg.Dispatch.MaxWorkerCount() != 8 || !slices.Equal(cfg.Dispatch.ProviderOrder, []string{"burst", "fast", "eu"}) {
		t.Fatalf("configuration not preserved: %+v", cfg)
	}
	if cfg.ProviderTemplates()["fast"].Snapshot != "golden" || cfg.ProviderTemplates()["burst"].Snapshot != "burst-base" || cfg.ProviderSizes()["burst"] != "xlarge" || cfg.ProviderSizes()["fast"] != "large" {
		t.Fatal("allocation profile lost")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeConfig(t, string(encoded))); err != nil {
		t.Fatalf("serialized configuration cannot be loaded: %v", err)
	}
}

func TestAgentCatalog(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		name, old, new string
		want           []Agent
	}{
		{"default", "", "", []Agent{{Driver: "codex", Models: []string{DefaultModel}}}},
		{"legacy model", `"t3Binary":`, `"model":"gpt-6.1","t3Binary":`, []Agent{{Driver: "codex", Models: []string{"gpt-6.1"}}}},
		{"declared agents", `"t3Binary":`, `"agents":[{"driver":"pi","models":["deepseek/v4"]},{"driver":"claudeAgent","models":["claude-opus-5-5","claude-sonnet-5-5"]},{"driver":"codex","models":["gpt-5.4"]}],"t3Binary":`,
			[]Agent{{Driver: "pi", Models: []string{"deepseek/v4"}}, {Driver: "claudeAgent", Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}}, {Driver: "codex", Models: []string{"gpt-5.4"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, strings.Replace(fixture, tc.old, tc.new, 1)))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Dispatch.AgentCatalog()
			if !slices.EqualFunc(got, tc.want, func(a, b Agent) bool { return a.Driver == b.Driver && slices.Equal(a.Models, b.Models) }) {
				t.Fatalf("catalog = %+v, want %+v", got, tc.want)
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(writeConfig(t, string(encoded))); err != nil {
				t.Fatalf("serialized configuration cannot be loaded: %v", err)
			}
		})
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"missing wallet", `"organization":"fast-org",`, ""},
		{"missing tenant", `"tenantId":"acme",`, ""},
		{"invalid wallet", `"organization":"fast-org"`, `"organization":" "`},
		{"invalid tenant", `"tenantId":"acme"`, `"tenantId":" "`},
		{"duplicate name", `"fast":{`, `"fast":{},"fast":{`},
		{"alias without driver", `"type":"boat",`, ""},
		{"unrecognized driver", `"type":"boat"`, `"type":"custom"`},
		{"wrong driver field even when zero", `"type":"boat"`, `"type":"boat","virtualCPU":0`},
		{"unknown nested template field", `"snapshot":"golden"`, `"snapshot":"golden","typo":true`},
		{"empty alias", `"burst":{`, `"":{`},
		{"dangling reference", `["burst","fast","eu"]`, `["missing"]`},
		{"repeated reference", `["burst","fast","eu"]`, `["fast","fast"]`},
		{"empty order", `["burst","fast","eu"]`, `[]`},
		{"missing order", `,"providerOrder":["burst","fast","eu"]`, ""},
		{"no dispatch", `,"dispatch":`, `,"oldDispatch":`},
		{"public dispatch", `127.0.0.1:47733`, `0.0.0.0:47733`},
		{"admin collision", `127.0.0.1:47733`, `127.0.0.1:47730`},
		{"invalid admin", `"dispatch":{`, `"listen":"0.0.0.0:1234","dispatch":{`},
		{"relative controller", `"/opt/t3"`, `"t3"`},
		{"invalid model", `"t3Binary":`, `"model":"wrong model","t3Binary":`},
		{"negative capacity", `"t3Binary":`, `"maxWorkers":-1,"t3Binary":`},
		{"empty agents", `"t3Binary":`, `"agents":[],"t3Binary":`},
		{"model with agents", `"t3Binary":`, `"model":"gpt-5.4","agents":[{"driver":"codex","models":["gpt-5.4"]}],"t3Binary":`},
		{"unknown agent driver", `"t3Binary":`, `"agents":[{"driver":"cursor","models":["auto"]}],"t3Binary":`},
		{"repeated agent driver", `"t3Binary":`, `"agents":[{"driver":"pi","models":["a"]},{"driver":"pi","models":["b"]}],"t3Binary":`},
		{"agent without models", `"t3Binary":`, `"agents":[{"driver":"pi","models":[]}],"t3Binary":`},
		{"invalid agent model", `"t3Binary":`, `"agents":[{"driver":"pi","models":["bad model"]}],"t3Binary":`},
		{"repeated agent model", `"t3Binary":`, `"agents":[{"driver":"pi","models":["a","a"]}],"t3Binary":`},
		{"unknown agent field", `"t3Binary":`, `"agents":[{"driver":"pi","models":["a"],"instance":"x"}],"t3Binary":`},
		{"unknown Boat size", `"large"`, `"xl"`},
		{"unknown Namespace size", `"virtualCPU":12,"memoryMB":32768`, `"size":"large"`},
		{"Namespace size and custom CPU", `"virtualCPU":12`, `"size":"l","virtualCPU":12`},
		{"negative RAM", `"memoryMB":32768`, `"memoryMB":-1`},
		{"Namespace architecture", `"arm64"`, `"sparc"`},
		{"unusable image", `"registered:acme/t3"`, `"registered:"`},
		{"empty template", `"snapshot":"golden"`, ""},
		{"conflicting template", `"snapshot":"golden"`, `"snapshot":"golden","image":"image"`},
		{"Namespace source without snapshot", `"image":"registered:acme/t3"`, `"source":"base"`},
		{"Namespace snapshot without source", `"image":"registered:acme/t3"`, `"snapshot":"id"`},
		{"relative token", `"/run/creds/namespace"`, `"token"`},
		{"conflicting credential selectors", `"apiKeyEnv":"FAST_TOKEN"`, `"apiKeyEnv":"FAST_TOKEN","apiKeyFile":"/run/key"`},
		{"relative credential", `"apiKeyEnv":"FAST_TOKEN"`, `"apiKeyFile":"key"`},
		{"raw secret", `"apiKeyEnv":"FAST_TOKEN"`, `"apiKey":"never-print-this"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, strings.Replace(fixture, tc.old, tc.new, 1)))
			if err == nil || strings.Contains(err.Error(), "never-print-this") {
				t.Fatal("configuration was accepted or error leaked input")
			}
		})
	}
	for _, input := range []string{`{}`, `null`, `[]`, fixture + fixture, `{"dispatchPOC":{"workers":["a","b"]}}`, `{"dispatchPOC":{"onDemand":{}}}`, `{"templates":{"boat":{"snapshot":"golden"}}}`} {
		if _, err := Load(writeConfig(t, input)); err == nil {
			t.Fatal("missing, old, or invalid configuration accepted")
		}
	}
}

func TestValidateProviderEndpointsWithoutCredentials(t *testing.T) {
	for _, tc := range []struct{ instance, field, endpoint string }{
		{"fast", "baseURL", "not-an-absolute-url"},
		{"fast", "baseURL", "https://user:never-print-this@example.org/api/v1"},
		{"fast", "baseURL", "https://example.org/api/v1?token=never-print-this"},
		{"fast", "baseURL", "http://example.org"},
		{"eu", "endpoint", "https://example.org/devbox"},
		{"eu", "endpoint", "https://example.org#never-print-this"},
		{"eu", "endpoint", "https://:443"},
		{"eu", "endpoint", "https://api.example:65536"},
		{"eu", "endpoint", "ftp://api.example"},
	} {
		t.Run(tc.instance+"_"+tc.field, func(t *testing.T) {
			// No credentials are available: Load and validate must report the
			// configuration error before attempting to resolve any login.
			raw := strings.Replace(fixture, `"`+tc.instance+`":{`, `"`+tc.instance+`":{"`+tc.field+`":"`+tc.endpoint+`",`, 1)
			_, err := Load(writeConfig(t, raw))
			if err == nil || !strings.Contains(err.Error(), "providers."+tc.instance+": "+tc.field) || strings.Contains(err.Error(), "never-print-this") {
				t.Fatalf("expected redacted endpoint error, got: %v", err)
			}
		})
	}
}

func TestProviderScopeTracksConnectionWithoutPinningSecrets(t *testing.T) {
	cfg, err := Load(writeConfig(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAST_TOKEN", "original")
	t.Setenv("OTHER_TOKEN", "original")
	scope := func(p ProviderConfig) string {
		t.Helper()
		cfg.Providers = map[string]ProviderConfig{"target": p}
		clients, err := cfg.BuildProviders()
		if err != nil {
			t.Fatal(err)
		}
		return clients["target"].(provider.ScopeIdentifier).ScopeID()
	}
	b, n := cfg.Providers["fast"], cfg.Providers["eu"]
	b.BaseURL, b.Organization = "https://BOAT.example:443/api/v1/", "first-org"
	first := scope(b)
	t.Setenv("FAST_TOKEN", "rotated")
	b.BaseURL = "https://boat.example/api/v1"
	if first != scope(b) {
		t.Fatal("credential rotation or equivalent endpoint spelling changed account scope")
	}
	b.Organization = "second-org"
	if first == scope(b) {
		t.Fatal("new wallet still owns previous reservation")
	}
	b.Organization = "first-org"
	b.BaseURL = "https://other.example/api/v1"
	if first == scope(b) {
		t.Fatal("new control plane still owns previous reservation")
	}
	b.BaseURL = "https://boat.example/api/v1"
	b.APIKeyEnv = "OTHER_TOKEN"
	if first != scope(b) {
		t.Fatal("rotation of the credential selector changed the wallet scope")
	}
	ns := scope(n)
	n.VirtualCPU = 99
	if ns != scope(n) {
		t.Fatal("changing shape changed account scope")
	}
	n.TokenFile = "/run/credentials/new-account"
	if ns != scope(n) {
		t.Fatal("credential selector changed tenant identity")
	}
	n.TenantID = "other-tenant"
	if ns == scope(n) {
		t.Fatal("a different Namespace tenant kept the same scope")
	}
}

func TestIndependentBoatInstances(t *testing.T) {
	t.Setenv("BOAT_API_KEY", "ambient")
	t.Setenv("FAST_TOKEN", "token-fast")
	t.Setenv("BURST_TOKEN", "token-burst")
	cfg, err := Load(writeConfig(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{"ok":true,"sandboxes":[]}`))
			if err != nil {
				t.Error(err)
			}
		case http.MethodPost:
			var body struct{ Type, From string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			seen = append(seen, strings.Join([]string{r.Header.Get("Authorization"), r.Header.Get("X-Boat-Org"), body.From, body.Type}, "/"))
			_, err := w.Write([]byte(`{"ok":true,"sandbox":{"id":"box","name":"box","state":"running"}}`))
			if err != nil {
				t.Error(err)
			}
		}
	}))
	defer srv.Close()
	for _, name := range []string{"fast", "burst"} {
		p := cfg.Providers[name]
		p.BaseURL = srv.URL
		p.Organization = "org-" + name
		cfg.Providers[name] = p
	}
	ps, err := cfg.BuildProviders()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fast", "burst"} {
		if ps[name].Name() != "boat" {
			t.Fatal("lost driver kind")
		}
		if _, err := ps[name].Create(t.Context(), provider.CreateRequest{Name: "worker", OperationID: name, Size: cfg.ProviderSizes()[name], Template: cfg.ProviderTemplates()[name]}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(seen, []string{"Bearer token-fast/org-fast/golden/large", "Bearer token-burst/org-burst/burst-base/xlarge"}) {
		t.Fatalf("wrong per-instance requests: %v", seen)
	}
}

func TestBoatCredentialsPriorityAndRedaction(t *testing.T) {
	t.Setenv("BOAT_API_KEY", "from-env")
	key, err := (Boat{Organization: "wallet"}).apiKey()
	if err != nil || key != "from-env" {
		t.Fatalf("env credentials: %q %v", key, err)
	}
	t.Setenv("BOAT_API_KEY", "ambient-must-not-override-explicit-file")
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err = (Boat{APIKeyFile: file}).apiKey()
	if err != nil || key != "from-file" {
		t.Fatalf("file credentials: %q %v", key, err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = (Boat{APIKeyFile: file}).apiKey()
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
	if _, err = (Boat{APIKeyFile: link}).apiKey(); err == nil {
		t.Fatal("symlink key accepted")
	}
	saved := writeConfig(t, `{"token":"cli-session","org":"saved-wallet"}`)
	key, err = (Boat{CredentialFile: saved, Organization: "explicit-wallet"}).apiKey()
	if err != nil || key != "cli-session" {
		t.Fatalf("saved credentials: %v", err)
	}
}

func TestMissingBackupCredentialDoesNotBlockFleetOrAnotherAccount(t *testing.T) {
	cfg, err := Load(writeConfig(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAST_TOKEN", "primary-key")
	t.Setenv("BURST_TOKEN", "")
	t.Setenv("NSC_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-sdk-login"))
	seen := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Boat-Org") + "/" + r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true,"sandbox":{"id":"worker","name":"worker","state":"running"}}`))
	}))
	defer srv.Close()
	for _, id := range []string{"fast", "burst"} {
		b := cfg.Providers[id]
		b.BaseURL = srv.URL
		cfg.Providers[id] = b
	}
	ns := cfg.Providers["eu"]
	ns.TokenFile = ""
	cfg.Providers["eu"] = ns
	clients, err := cfg.BuildProviders()
	if err != nil {
		t.Fatalf("reading unused provider credentials at startup: %v", err)
	}
	if _, err := clients["fast"].Get(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "fast-org/Bearer primary-key" {
		t.Fatalf("unexpected primary account: %q", got)
	}

	if _, err := clients["burst"].Get(t.Context(), "worker"); err == nil || errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("absent credentials must not report machine absence: %v", err)
	}
	if _, err := clients["burst"].Create(t.Context(), provider.CreateRequest{Name: "worker", OperationID: "workers-operation", Template: cfg.ProviderTemplates()["burst"]}); !errors.Is(err, provider.ErrCreateRejected) {
		t.Fatalf("local refusal before a create must allow first-attempt fallback: %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("contacted provider without credentials")
	}
	t.Setenv("BURST_TOKEN", "new-key")
	if _, err := clients["burst"].Get(t.Context(), "worker"); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "burst-org/Bearer new-key" {
		t.Fatalf("credential rotation switched wallet: %q", got)
	}
}
