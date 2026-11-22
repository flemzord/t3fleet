package config

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// DiscoverDefaultPath prefers a unique existing config, or the TOML path for
// a fresh installation. A broken link still counts as an existing choice.
func DiscoverDefaultPath() (string, error) {
	modern := DefaultPath()
	legacy := filepath.Join(filepath.Dir(modern), "config.json")
	_, modernErr := os.Lstat(modern)
	_, legacyErr := os.Lstat(legacy)
	if modernErr != nil && !errors.Is(modernErr, os.ErrNotExist) ||
		legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return "", errors.New("cannot inspect default configuration paths")
	}
	switch {
	case modernErr == nil && legacyErr == nil:
		return "", errors.New("both config.toml and config.json exist; select one with --config or T3FLEET_CONFIG")
	case legacyErr == nil:
		return legacy, nil
	default:
		return modern, nil
	}
}

// Files with a .toml suffix use TOML. All other paths retain the original JSON
// behavior, so scripts with custom config filenames continue to work.
func isTOML(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".toml")
}

func Load(path string) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration; create a config file and pass --config PATH")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, errors.New("configuration must be no larger than 1 MiB")
	}
	format := "JSON"
	if isTOML(path) {
		format = "TOML"
		data, err = tomlToJSON(data)
		if err != nil {
			// Decoder error text can contain a raw value or input line.
			return Config{}, errors.New("invalid configuration TOML; use tables, strings, integers, booleans, and arrays")
		}
	}
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) ||
		jsonv2.Unmarshal(data, &cfg, jsonv2.RejectUnknownMembers(true)) != nil {
		// Both formats use the same strict JSON schema, including the
		// driver-specific fields and sensitive-value redaction.
		return Config{}, errors.New("invalid configuration " + format + " or unknown field; use dispatch and named providers with type and template")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func tomlToJSON(data []byte) ([]byte, error) {
	var value map[string]any
	if err := toml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	// Do not let JSON conversion silently turn TOML dates into strings or
	// floating-point numbers into integers.
	if err := normalizeValues(value); err != nil {
		return nil, err
	}
	return jsonv2.Marshal(value)
}

// MarshalFile renders the same wire schema used by Load.
func MarshalFile(path string, cfg Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if !isTOML(path) {
		return append(data, '\n'), nil
	}
	var value map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := normalizeValues(value); err != nil {
		return nil, err
	}
	return toml.Marshal(value)
}

func normalizeValues(node any) error {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			switch v := child.(type) {
			case json.Number:
				n, err := v.Int64()
				if err != nil {
					return err
				}
				value[key] = n
			default:
				if err := normalizeValues(child); err != nil {
					return err
				}
			}
		}
	case []any:
		for _, child := range value {
			if err := normalizeValues(child); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, child := range value {
			if err := normalizeValues(child); err != nil {
				return err
			}
		}
	case string, int64, bool:
	default:
		return errors.New("unsupported configuration value")
	}
	return nil
}
