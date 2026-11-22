package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFixedDispatchConfiguration(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "fixed.toml"))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Dispatch
	if d.Mode != "fixed" || d.Fixed.Workers != 3 {
		t.Fatal("fixed settings were not loaded")
	}
	if idle, err := d.Fixed.IdleDuration(); err != nil || idle != 15*time.Minute {
		t.Fatalf("invalid idle timeout: %v, %v", idle, err)
	}
	for _, bad := range []Dispatch{
		{Mode: "unknown"},
		{Mode: "fixed"},
		{Fixed: &FixedDispatch{Workers: 3, IdleTimeout: "15m"}},
		{Mode: "fixed", Fixed: &FixedDispatch{Workers: 3, IdleTimeout: "59s"}},
		{Mode: "fixed", Fixed: &FixedDispatch{Workers: 101, IdleTimeout: "15m"}},
		{Mode: "fixed", MaxWorkers: 8, Fixed: d.Fixed},
		{Mode: "fixed", DeleteWorkerOnArchive: true, Fixed: d.Fixed},
	} {
		if bad.validateMode() == nil {
			t.Errorf("accepted an invalid dispatch mode/config: %+v", bad)
		}
	}
}
