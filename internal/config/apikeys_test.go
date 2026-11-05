package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIKeysFallback(t *testing.T) {
	dir := t.TempDir()

	// 新配置：只有 api_keys
	p1 := filepath.Join(dir, "new.json")
	if err := os.WriteFile(p1, []byte(`{"port":"8080","api_keys":["k1","k2"],"default_model":"mimo-v2.6-pro","accounts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p1)
	if err != nil {
		t.Fatalf("load new: %v", err)
	}
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[0] != "k1" {
		t.Fatalf("api_keys not loaded: %+v", cfg.APIKeys)
	}
	if cfg.APIKey != "k1" {
		t.Fatalf("api_key should mirror the first key, got %q", cfg.APIKey)
	}

	// 老配置：只有 api_key
	p2 := filepath.Join(dir, "old.json")
	if err := os.WriteFile(p2, []byte(`{"port":"8080","api_key":"legacy","default_model":"mimo-v2.6-pro","accounts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatalf("load old: %v", err)
	}
	if len(cfg2.APIKeys) != 1 || cfg2.APIKeys[0] != "legacy" {
		t.Fatalf("legacy api_key should be promoted to api_keys: %+v", cfg2.APIKeys)
	}
}
