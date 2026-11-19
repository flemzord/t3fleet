// Package config loads explicit, non-secret daemon configuration.
package config

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/flemzord/t3fleet/pkg/provider"
)

const DefaultVersion = "0.1.11-nightly.20261117.4838"

type Config struct {
	Listen          string                    `json:"listen"`
	StateDir        string                    `json:"stateDir"`
	ExpectedVersion string                    `json:"expectedVersion"`
	Dispatch        *Dispatch                 `json:"dispatch"`
	Providers       map[string]ProviderConfig `json:"providers"`
}

// Dispatch describes the T3 server that allocates one worker for each thread.
// ProviderOrder names configured instances, not adapter types.
type Dispatch struct {
	Listen                string   `json:"listen"`
	T3Binary              string   `json:"t3Binary"`
	ProviderOrder         []string `json:"providerOrder"`
	MaxWorkers            int      `json:"maxWorkers,omitempty"`
	Model                 string   `json:"model,omitempty"`
	Agents                []Agent  `json:"agents,omitempty"`
	DeleteWorkerOnArchive bool     `json:"deleteWorkerOnArchive,omitempty"`
}

// Agent declares one T3 provider driver and the models the fleet offers for
// it. The T3 instance ID is the driver name, as on a default T3 server.
type Agent struct {
	Driver string   `json:"driver"`
	Models []string `json:"models"`
}

// DefaultModel is the Codex model offered when no agent or model is set.
const DefaultModel = "gpt-5.4"

// AgentDrivers lists the T3 drivers a worker image can run.
var AgentDrivers = []string{"codex", "claudeAgent", "pi"}

// AgentCatalog returns the declared agents. Without dispatch.agents it offers
// the single Codex model from dispatch.model. The first model is the default.
func (d Dispatch) AgentCatalog() []Agent {
	if len(d.Agents) != 0 {
		return d.Agents
	}
	if d.Model != "" {
		return []Agent{{Driver: "codex", Models: []string{d.Model}}}
	}
	return []Agent{{Driver: "codex", Models: []string{DefaultModel}}}
}

func (d Dispatch) MaxWorkerCount() int {
	if d.MaxWorkers == 0 {
		return 8
	}
	return d.MaxWorkers
}

func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(dir, "t3fleet", "config.json")
}

func Default() Config {
	home, _ := os.UserHomeDir()
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".local", "state")
	}
	return Config{Listen: "127.0.0.1:47730", StateDir: filepath.Join(dir, "t3fleet"), ExpectedVersion: DefaultVersion, Providers: map[string]ProviderConfig{}}
}

func Load(path string) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration; create a config file and pass --config PATH")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return Config{}, errors.New("configuration must be a JSON object no larger than 1 MiB")
	}
	if err := jsonv2.Unmarshal(data, &cfg, jsonv2.RejectUnknownMembers(true)); err != nil {
		// An offending key or value may contain a secret; never echo raw input.
		return Config{}, errors.New("invalid configuration JSON or unknown field; use dispatch and named providers with type and template")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func (cfg Config) Validate() error {
	admin, err := netip.ParseAddrPort(cfg.Listen)
	if err != nil || !admin.Addr().IsLoopback() || admin.Port() == 0 {
		return errors.New("listen must be a loopback IP and a nonzero port")
	}
	if !filepath.IsAbs(cfg.StateDir) || filepath.Clean(cfg.StateDir) == string(filepath.Separator) {
		return errors.New("stateDir must be an absolute, non-root directory")
	}
	if cfg.ExpectedVersion == "" || strings.ContainsAny(cfg.ExpectedVersion, "\r\n\x00 ") {
		return errors.New("expectedVersion is required and must be a T3 version string")
	}
	d := cfg.Dispatch
	if d == nil {
		return errors.New("dispatch is required; configure an on-demand T3 server")
	}
	dispatch, err := netip.ParseAddrPort(d.Listen)
	if err != nil || dispatch.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 1}) || dispatch.Port() == 0 || dispatch == netip.AddrPortFrom(admin.Addr().Unmap(), admin.Port()) {
		return errors.New("dispatch.listen must use 127.0.0.1 and a separate, nonzero port")
	}
	if !filepath.IsAbs(d.T3Binary) || strings.ContainsAny(d.T3Binary, "\x00\r\n") {
		return errors.New("dispatch.t3Binary must be an absolute executable path")
	}
	if d.MaxWorkers < 0 {
		return errors.New("dispatch.maxWorkers must be positive or zero for the default")
	}
	if d.Model != "" && !validModel(d.Model) {
		return errors.New("dispatch.model must be a Codex model identifier")
	}
	if err := validateAgents(d); err != nil {
		return err
	}
	if len(d.ProviderOrder) == 0 {
		return errors.New("dispatch.providerOrder must contain at least one configured instance name")
	}
	for name, instance := range cfg.Providers {
		if !instanceName.MatchString(name) {
			return errors.New("provider instance names must start with a lowercase letter and contain at most 63 lowercase letters, digits, underscores or hyphens")
		}
		if err := instance.validate(); err != nil {
			return fmt.Errorf("providers.%s: %w", name, err)
		}
	}
	seen := make(map[string]bool)
	for _, name := range d.ProviderOrder {
		if _, ok := cfg.Providers[name]; !ok {
			return errors.New("dispatch.providerOrder names an unconfigured provider instance")
		}
		if seen[name] {
			return fmt.Errorf("dispatch.providerOrder repeats %q", name)
		}
		seen[name] = true
	}
	return nil
}

func validModel(model string) bool {
	return model != "" && len(model) <= 128 && !strings.ContainsAny(model, " \t\r\n\x00")
}

func validateAgents(d *Dispatch) error {
	if d.Agents == nil {
		return nil
	}
	if d.Model != "" {
		return errors.New("dispatch.model and dispatch.agents are exclusive; list the Codex model under dispatch.agents")
	}
	if len(d.Agents) == 0 {
		return errors.New("dispatch.agents must declare at least one agent")
	}
	drivers := make(map[string]bool)
	for _, agent := range d.Agents {
		if !slices.Contains(AgentDrivers, agent.Driver) {
			return fmt.Errorf("dispatch.agents driver must be one of %s", strings.Join(AgentDrivers, ", "))
		}
		if drivers[agent.Driver] {
			return fmt.Errorf("dispatch.agents repeats driver %q", agent.Driver)
		}
		drivers[agent.Driver] = true
		if len(agent.Models) == 0 {
			return fmt.Errorf("dispatch.agents.%s must list at least one model", agent.Driver)
		}
		models := make(map[string]bool)
		for _, model := range agent.Models {
			if !validModel(model) {
				return fmt.Errorf("dispatch.agents.%s models must be identifiers of at most 128 characters without spaces", agent.Driver)
			}
			if models[model] {
				return fmt.Errorf("dispatch.agents.%s repeats model %q", agent.Driver, model)
			}
			models[model] = true
		}
	}
	return nil
}

func (cfg Config) ProviderTemplates() map[string]provider.Template {
	out := make(map[string]provider.Template, len(cfg.Providers))
	for name, p := range cfg.Providers {
		out[name] = p.Template
	}
	return out
}

func (cfg Config) ProviderSizes() map[string]string {
	out := make(map[string]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		out[name] = p.Size
	}
	return out
}
