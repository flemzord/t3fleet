package config

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/flemzord/t3fleet/pkg/provider"
	"github.com/flemzord/t3fleet/pkg/provider/boat"
	"github.com/flemzord/t3fleet/pkg/provider/namespace"
)

// ProviderConfig is one independently named allocation target. Only the
// selected driver can consume credential and placement settings.
type ProviderConfig struct {
	Type     string            `json:"type"`
	Size     string            `json:"size,omitempty"`
	Template provider.Template `json:"template"`
	Boat
	Namespace
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
	TenantID     string `json:"tenantId,omitempty"`
	TokenFile    string `json:"tokenFile,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Site         string `json:"site,omitempty"`
	VirtualCPU   int32  `json:"virtualCPU,omitempty"`
	MemoryMB     int32  `json:"memoryMB,omitempty"`
	Architecture string `json:"architecture,omitempty"`
}

// Decode using a driver-specific schema. A field for another driver is an
// error even when its value is an empty string or zero.
func (p *ProviderConfig) UnmarshalJSON(data []byte) error {
	type common struct {
		Type     string            `json:"type"`
		Size     string            `json:"size,omitempty"`
		Template provider.Template `json:"template"`
	}
	var head common
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	var opts ProviderConfig
	switch head.Type {
	case "boat":
		var wire struct {
			common
			Boat
		}
		if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		opts.Boat = wire.Boat
	case "namespace":
		var wire struct {
			common
			Namespace
		}
		if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		opts.Namespace = wire.Namespace
	default:
		return errors.New("provider type must be boat or namespace")
	}
	opts.Type, opts.Size, opts.Template = head.Type, head.Size, head.Template
	*p = opts
	return nil
}

func (p ProviderConfig) validate() error {
	t := p.Template
	for _, value := range []string{t.Source, t.Snapshot, t.Image} {
		if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid template value")
		}
	}
	switch p.Type {
	case "boat":
		if _, err := boat.NormalizeBaseURL(p.BaseURL); err != nil {
			return fmt.Errorf("baseURL: %w", err)
		}
		if !provider.ValidAccountID(p.Organization) {
			return errors.New("Boat requires an explicit organization")
		}
		if strings.ContainsAny(p.Environment, "\x00\r\n") {
			return errors.New("invalid Boat organization or environment")
		}
		if p.Namespace != (Namespace{}) {
			return errors.New("Namespace settings on a Boat provider")
		}
		if t.Snapshot == "" || t.Image != "" || t.Source != "" {
			return errors.New("Boat requires template.snapshot (without image or source)")
		}
		if !boat.ValidSize(p.Size) {
			return errors.New("Boat size must be small, default, large, or xlarge")
		}
		if p.APIKeyEnv != "" && strings.ContainsAny(p.APIKeyEnv, "=\x00\r\n") {
			return errors.New("invalid Boat API key environment name")
		}
		var selectors int
		for _, value := range []string{p.APIKeyEnv, p.APIKeyFile, p.CredentialFile} {
			if value != "" {
				selectors++
			}
		}
		if selectors > 1 {
			return errors.New("choose at most one of apiKeyEnv, apiKeyFile, or credentialFile")
		}
		for _, file := range []string{p.APIKeyFile, p.CredentialFile} {
			if file != "" && !filepath.IsAbs(file) {
				return errors.New("credential file paths must be absolute")
			}
		}
	case "namespace":
		if _, err := namespace.NormalizeEndpoint(p.Endpoint); err != nil {
			return fmt.Errorf("endpoint: %w", err)
		}
		if !provider.ValidAccountID(p.TenantID) {
			return errors.New("Namespace requires an explicit tenantId")
		}
		if p.Boat != (Boat{}) {
			return errors.New("Boat settings on a Namespace provider")
		}
		if (t.Image == "") == (t.Source == "") || t.Image == "registered:" || ((t.Snapshot == "") != (t.Source == "")) {
			return errors.New("Namespace requires template.image or template.source with an explicit snapshot")
		}
		if p.TokenFile != "" && !filepath.IsAbs(p.TokenFile) {
			return errors.New("tokenFile must be an absolute path")
		}
		if !namespace.ValidSize(p.Size) {
			return errors.New("Namespace size must be s, m, l, or xl")
		}
		if p.VirtualCPU < 0 || p.MemoryMB < 0 || (p.Architecture != "" && p.Architecture != "amd64" && p.Architecture != "arm64") {
			return errors.New("invalid Namespace machine shape")
		}
		if p.Size != "" && (p.VirtualCPU != 0 || p.MemoryMB != 0) {
			return errors.New("choose size or virtualCPU/memoryMB")
		}
	default:
		return errors.New("provider type must be boat or namespace")
	}
	return nil
}

// BuildProviders keeps the instance ID (map key) separate from the concrete
// driver's Name. Credentials are resolved only when using an instance.
func (cfg Config) BuildProviders() (map[string]provider.Provider, error) {
	if len(cfg.Providers) == 0 {
		return nil, errors.New("configure at least one named provider")
	}
	out := make(map[string]provider.Provider, len(cfg.Providers))
	for name, p := range cfg.Providers {
		switch p.Type {
		case "boat":
			client, err := boat.New(boat.Config{APIKeySource: p.apiKey, Organization: p.Organization, Environment: p.Environment, BaseURL: p.BaseURL})
			if err != nil {
				return nil, fmt.Errorf("cannot initialize Boat instance %q; check credentials, organization and API URL", name)
			}
			out[name] = client
		case "namespace":
			client, err := namespace.New(namespace.Config{TenantID: p.TenantID, TokenFile: p.TokenFile, Endpoint: p.Endpoint, Site: p.Site, VirtualCPU: p.VirtualCPU, MemoryMB: p.MemoryMB, Architecture: p.Architecture})
			if err != nil {
				return nil, fmt.Errorf("cannot initialize Namespace instance %q; check endpoint, credentials and machine shape", name)
			}
			out[name] = client
		default:
			return nil, errors.New("unsupported provider type")
		}
	}
	return out, nil
}
