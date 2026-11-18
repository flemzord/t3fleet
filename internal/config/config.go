// Package config loads explicit, non-secret daemon configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
	"github.com/flemzord/t3fleet/pkg/provider/boat"
	"github.com/flemzord/t3fleet/pkg/provider/namespace"
)

const DefaultVersion = "0.1.11-nightly.20261117.4838"

type Config struct {
	DispatchPOC     *DispatchPOC                 `json:"dispatchPOC,omitempty"`
	IdleTimeout     string                       `json:"idleTimeout,omitempty"`
	Listen          string                       `json:"listen"`
	StateDir        string                       `json:"stateDir"`
	MachineHost     string                       `json:"machineHost"`
	ExpectedVersion string                       `json:"expectedVersion"`
	Providers       ProviderConfigs              `json:"providers"`
	Templates       map[string]provider.Template `json:"templates"`
}

type DispatchPOC struct {
	Listen   string            `json:"listen"`
	Workers  []string          `json:"workers,omitzero"`
	OnDemand *DispatchOnDemand `json:"onDemand,omitempty"`
}

type DispatchOnDemand struct {
	T3Binary   string `json:"t3Binary"`
	MaxWorkers int    `json:"maxWorkers,omitempty"`
	Model      string `json:"model,omitempty"`
}

func (d DispatchOnDemand) MaxWorkerCount() int {
	if d.MaxWorkers == 0 {
		return 8
	}
	return d.MaxWorkers
}

type ProviderConfigs struct {
	Boat      *Boat      `json:"boat,omitempty"`
	Namespace *Namespace `json:"namespace,omitempty"`
}

type Boat struct {
	APIKeyEnv      string `json:"apiKeyEnv,omitempty"`
	APIKeyFile     string `json:"apiKeyFile,omitempty"`
	CredentialFile string `json:"credentialFile,omitempty"`
	Organization   string `json:"organization,omitempty"`
	Environment    string `json:"environment,omitempty"`
	BaseURL        string `json:"baseURL,omitempty"`
}

type Namespace struct {
	Endpoint     string `json:"endpoint,omitempty"`
	Site         string `json:"site,omitempty"`
	VirtualCPU   int32  `json:"virtualCPU,omitempty"`
	MemoryMB     int32  `json:"memoryMB,omitempty"`
	Architecture string `json:"architecture,omitempty"`
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
	return Config{Listen: "127.0.0.1:47730", MachineHost: "127.0.0.1", StateDir: filepath.Join(dir, "t3fleet"), ExpectedVersion: DefaultVersion, Templates: map[string]provider.Template{}}
}

func Load(path string) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration; create a config file and pass --config PATH")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return Config{}, errors.New("configuration must be a JSON object smaller than 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid configuration JSON or unknown field")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Config{}, errors.New("configuration must contain one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) IdleDuration() (time.Duration, error) {
	if cfg.IdleTimeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(cfg.IdleTimeout)
	if err != nil || d < 0 || d > 0 && d < time.Minute {
		return 0, errors.New("idleTimeout must be 0 or a duration of at least 1m")
	}
	return d, nil
}

func (cfg Config) Validate() error {
	idle, err := cfg.IdleDuration()
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("listen must be a loopback host:port")
	}
	if !loopback(host) {
		return errors.New("admin listen address must be loopback")
	}
	if poc := cfg.DispatchPOC; poc != nil {
		host, port, splitErr := net.SplitHostPort(poc.Listen)
		n, portErr := strconv.Atoi(port)
		if splitErr != nil || portErr != nil || host != "127.0.0.1" || n < 1 || n > 65535 || poc.Listen == cfg.Listen {
			return errors.New("dispatchPOC.listen must be a separate 127.0.0.1 port")
		}
		if idle != 0 {
			return errors.New("dispatchPOC requires idleTimeout disabled")
		}
		if onDemand := poc.OnDemand; onDemand != nil {
			if len(poc.Workers) != 0 || cfg.Providers.Boat == nil || cfg.Templates["boat"] == (provider.Template{}) {
				return errors.New("dispatchPOC.onDemand requires a Boat template and no dedicated workers")
			}
			if !filepath.IsAbs(onDemand.T3Binary) || strings.ContainsAny(onDemand.T3Binary, "\x00\r\n") {
				return errors.New("dispatchPOC.onDemand.t3Binary must be an absolute executable path")
			}
			if onDemand.MaxWorkerCount() < 1 || onDemand.MaxWorkerCount() > 8 {
				return errors.New("dispatchPOC.onDemand.maxWorkers must be between 1 and 8")
			}
			if len(onDemand.Model) > 128 || strings.ContainsAny(onDemand.Model, " \t\r\n\x00") {
				return errors.New("dispatchPOC.onDemand.model must be a Codex model identifier")
			}
		} else if len(poc.Workers) < 2 || len(poc.Workers) > 8 {
			return errors.New("dispatchPOC requires 2–8 dedicated workers or onDemand")
		}
	}
	if cfg.MachineHost != "127.0.0.1" {
		return errors.New("machineHost must be 127.0.0.1 in this release")
	}
	if !filepath.IsAbs(cfg.StateDir) || filepath.Clean(cfg.StateDir) == string(filepath.Separator) {
		return errors.New("stateDir must be an absolute, non-root directory")
	}
	if cfg.ExpectedVersion == "" || strings.ContainsAny(cfg.ExpectedVersion, "\r\n\x00 ") {
		return errors.New("expectedVersion is required and must be a T3 version string")
	}
	for name, t := range cfg.Templates {
		if name != "boat" && name != "namespace" {
			return fmt.Errorf("unknown template provider %q", name)
		}
		if t.Image != "" && (t.Snapshot != "" || t.Source != "") {
			return errors.New("template image cannot be combined with snapshot/source")
		}
		if name == "boat" && t.Source != "" {
			return errors.New("Boat templates do not use source")
		}
		if name == "namespace" && t.Snapshot != "" && t.Source == "" {
			return errors.New("Namespace snapshots require a source Devbox")
		}
	}
	if cfg.Providers.Boat != nil {
		b := cfg.Providers.Boat
		if b.APIKeyEnv != "" && strings.ContainsAny(b.APIKeyEnv, "=\x00\r\n") {
			return errors.New("invalid Boat API key environment name")
		}
		for _, p := range []string{b.APIKeyFile, b.CredentialFile} {
			if p != "" && !filepath.IsAbs(p) {
				return errors.New("credential file paths must be absolute")
			}
		}
	}
	return nil
}

func loopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }

// BuildProviders resolves credentials at daemon startup. CLI-only operations
// never require cloud credentials. Secrets are not part of Config or its JSON.
func (cfg Config) BuildProviders() (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	if cfg.Providers.Boat != nil {
		key, org, err := cfg.Providers.Boat.credentials()
		if err != nil {
			return nil, err
		}
		client, err := boat.New(boat.Config{APIKey: key, Organization: org, Environment: cfg.Providers.Boat.Environment, BaseURL: cfg.Providers.Boat.BaseURL})
		if err != nil {
			return nil, errors.New("cannot initialize Boat; check credentials, organization and API URL")
		}
		out["boat"] = client
	}
	if n := cfg.Providers.Namespace; n != nil {
		client, err := namespace.New(namespace.Config{Endpoint: n.Endpoint, Site: n.Site, VirtualCPU: n.VirtualCPU, MemoryMB: n.MemoryMB, Architecture: n.Architecture})
		if err != nil {
			return nil, errors.New("cannot initialize Namespace; check the current Namespace login or NSC_TOKEN_FILE")
		}
		out["namespace"] = client
	}
	if len(out) == 0 {
		return nil, errors.New("configure at least one provider under providers.boat or providers.namespace")
	}
	return out, nil
}

func (b Boat) credentials() (string, string, error) {
	env := b.APIKeyEnv
	if env == "" {
		env = "BOAT_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(env))
	org := b.Organization
	if key == "" && b.APIKeyFile != "" {
		data, err := privateRead(b.APIKeyFile)
		if err != nil {
			return "", "", errors.New("cannot read Boat API key file; require a regular private file")
		}
		key = strings.TrimSpace(string(data))
	}
	if key == "" {
		file := b.CredentialFile
		if file == "" {
			dir, err := os.UserConfigDir()
			if err == nil {
				file = filepath.Join(dir, "ascii", "boat", "config.json")
			}
		}
		data, err := privateRead(file)
		if err != nil {
			return "", "", errors.New("Boat credentials unavailable; set BOAT_API_KEY or apiKeyFile, or sign in with Boat once")
		}
		var saved struct {
			Token string `json:"token"`
			Org   string `json:"org"`
		}
		if json.Unmarshal(data, &saved) != nil {
			return "", "", errors.New("invalid Boat credential file")
		}
		key = saved.Token
		if org == "" {
			org = saved.Org
		}
	}
	if key == "" || strings.ContainsAny(key+org, "\r\n\x00") {
		return "", "", errors.New("Boat credentials are empty or invalid")
	}
	// An empty organization follows the current Boat wallet. README documents
	// this explicitly; specifying organization makes billing deterministic.
	return key, org, nil
}

func privateRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("unsafe secret file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read secret file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secret file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil || len(data) == 64<<10 {
		return nil, errors.New("invalid secret file")
	}
	return data, nil
}
