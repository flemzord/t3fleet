// Package config loads explicit, non-secret daemon configuration.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
	Listen                string         `json:"listen"`
	ExternalOrigin        string         `json:"externalOrigin,omitempty"`
	T3Binary              string         `json:"t3Binary"`
	ProviderOrder         []string       `json:"providerOrder"`
	Mode                  string         `json:"mode,omitempty"`
	Fixed                 *FixedDispatch `json:"fixed,omitempty"`
	MaxWorkers            int            `json:"maxWorkers,omitempty"`
	Model                 string         `json:"model,omitempty"`
	Agents                []Agent        `json:"agents,omitempty"`
	DeleteWorkerOnArchive bool           `json:"deleteWorkerOnArchive,omitempty"`
}

type FixedDispatch struct {
	Workers     int    `json:"workers"`
	IdleTimeout string `json:"idleTimeout"`
}

const MaxFixedWorkers = 100

func (f FixedDispatch) IdleDuration() (time.Duration, error) {
	timeout, err := time.ParseDuration(f.IdleTimeout)
	if err != nil || timeout < time.Minute {
		return 0, errors.New("dispatch.fixed.idleTimeout must be a duration of at least 1m")
	}
	return timeout, nil
}

func (d Dispatch) validateMode() error {
	switch d.Mode {
	case "", "on-demand":
		if d.Fixed != nil {
			return errors.New("dispatch.fixed requires dispatch.mode = fixed")
		}
	case "fixed":
		if d.Fixed == nil || d.Fixed.Workers < 1 || d.Fixed.Workers > MaxFixedWorkers {
			return fmt.Errorf("dispatch.fixed.workers must be between 1 and %d", MaxFixedWorkers)
		}
		if d.MaxWorkers != 0 || d.DeleteWorkerOnArchive {
			return errors.New("dispatch.maxWorkers and dispatch.deleteWorkerOnArchive are only for on-demand workers")
		}
		if _, err := d.Fixed.IdleDuration(); err != nil {
			return err
		}
	default:
		return errors.New("dispatch.mode must be on-demand or fixed")
	}
	return nil
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
		return "config.toml"
	}
	return filepath.Join(dir, "t3fleet", "config.toml")
}

func Default() Config {
	home, _ := os.UserHomeDir()
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".local", "state")
	}
	return Config{Listen: "127.0.0.1:47730", StateDir: filepath.Join(dir, "t3fleet"), ExpectedVersion: DefaultVersion, Providers: map[string]ProviderConfig{}}
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
var originLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var numericHostLabel = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*)$`)

// ParseExternalOrigin returns the canonical HTTPS origin exposed by a TLS proxy.
func ParseExternalOrigin(raw string) (*url.URL, error) {
	invalid := errors.New("dispatch.externalOrigin must be an HTTPS origin with a non-loopback host and no path, query, or fragment")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsLoopback() || ip.IsUnspecified() || ip.Is4In6() || ip.Zone() != "" {
			return nil, invalid
		}
		host = ip.String()
		if ip.Is6() {
			host = "[" + host + "]"
		}
	} else {
		if strings.HasPrefix(u.Host, "[") || len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return nil, invalid
		}
		for label := range strings.SplitSeq(host, ".") {
			if !originLabel.MatchString(label) {
				return nil, invalid
			}
		}
		// Browsers interpret some numeric hostnames as legacy IPv4 literals.
		if numericHostLabel.MatchString(host[strings.LastIndexByte(host, '.')+1:]) {
			return nil, invalid
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return nil, invalid
		}
		if n != 443 {
			host += ":" + strconv.FormatUint(n, 10)
		}
	}
	return &url.URL{Scheme: "https", Host: host}, nil
}

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
	if err := d.validateMode(); err != nil {
		return err
	}
	dispatch, err := netip.ParseAddrPort(d.Listen)
	if err != nil || dispatch.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 1}) || dispatch.Port() == 0 || dispatch == netip.AddrPortFrom(admin.Addr().Unmap(), admin.Port()) {
		return errors.New("dispatch.listen must use 127.0.0.1 and a separate, nonzero port")
	}
	if d.ExternalOrigin != "" {
		if _, err := ParseExternalOrigin(d.ExternalOrigin); err != nil {
			return err
		}
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
