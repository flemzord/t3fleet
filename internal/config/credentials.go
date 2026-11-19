package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (b Boat) apiKey() (string, error) {
	var key string
	switch {
	case b.APIKeyEnv != "":
		key = strings.TrimSpace(os.Getenv(b.APIKeyEnv))
	case b.APIKeyFile != "":
		data, err := privateRead(b.APIKeyFile)
		if err != nil {
			return "", errors.New("cannot read Boat API key file; require a regular private file")
		}
		key = strings.TrimSpace(string(data))
	default:
		file := b.CredentialFile
		if file == "" {
			key = strings.TrimSpace(os.Getenv("BOAT_API_KEY"))
			if key != "" {
				break
			}
			dir, err := os.UserConfigDir()
			if err == nil {
				file = filepath.Join(dir, "ascii", "boat", "config.json")
			}
		}
		data, err := privateRead(file)
		if err != nil {
			return "", errors.New("Boat credentials unavailable; configure a private credential source or sign in with Boat")
		}
		var saved struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(data, &saved) != nil {
			return "", errors.New("invalid Boat credential file")
		}
		key = saved.Token
	}
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return "", errors.New("Boat credentials are empty or invalid")
	}
	return key, nil
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
	defer func() { _ = file.Close() }()
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
