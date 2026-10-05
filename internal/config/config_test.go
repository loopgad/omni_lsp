package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.LogLevel != "info" {
		t.Errorf("expected info, got %s", cfg.LogLevel)
	}
	if cfg.Transport != "stdio" {
		t.Errorf("expected stdio, got %s", cfg.Transport)
	}
	if cfg.MaxConcurrentRequests != 64 {
		t.Errorf("expected 64, got %d", cfg.MaxConcurrentRequests)
	}
	if cfg.RequestTimeoutMs != 5000 {
		t.Errorf("expected 5000, got %d", cfg.RequestTimeoutMs)
	}
	if cfg.IndexDir != "" {
		t.Errorf("expected empty index dir, got %q", cfg.IndexDir)
	}
	if want := int64(256 * 1024 * 1024); cfg.IndexDiskBudgetBytes != want {
		t.Errorf("expected %d index budget, got %d", want, cfg.IndexDiskBudgetBytes)
	}
}

func TestValidateValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestValidateInvalidTransport(t *testing.T) {
	cfg := Default()
	cfg.Transport = "ws"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid transport")
	}
}

func TestValidateZeroMaxConcurrent(t *testing.T) {
	cfg := Default()
	cfg.MaxConcurrentRequests = 0
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero MaxConcurrentRequests")
	}
}

func TestValidateEmptyBackendID(t *testing.T) {
	cfg := Default()
	cfg.Backends = []BackendConfig{{Enabled: true, LanguageID: ""}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty backend language ID")
	}
}

func TestValidateNegativeIndexDiskBudget(t *testing.T) {
	cfg := Default()
	cfg.IndexDiskBudgetBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative index disk budget")
	}
}

func TestValidateZeroIndexDiskBudget(t *testing.T) {
	cfg := Default()
	cfg.IndexDiskBudgetBytes = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero index disk budget should disable the limit: %v", err)
	}
}

func TestLoadIndexDirFromEnv(t *testing.T) {
	indexDir := t.TempDir()
	t.Setenv("OMNILSP_INDEX_DIR", indexDir)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("loading config with index dir env: %v", err)
	}
	if cfg.IndexDir != indexDir {
		t.Fatalf("expected index dir from env, got %q", cfg.IndexDir)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("loading missing optional file should not error: %v", err)
	}
	if cfg.Transport != "stdio" {
		t.Error("missing file should return defaults")
	}
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	_ = cfg
}

// TestDefaultConfigHasNoReadAPITokens pins the local-trust baseline: without
// configuration the token set is empty and no auth wrapper is installed.
func TestDefaultConfigHasNoReadAPITokens(t *testing.T) {
	cfg := Default()
	if len(cfg.ReadAPITokens) != 0 {
		t.Errorf("expected no default read API tokens, got %v", cfg.ReadAPITokens)
	}
	if cfg.ReadAPITokenSet() != nil {
		t.Errorf("expected nil token set for defaults, got %v", cfg.ReadAPITokenSet())
	}
}

func TestLoadReadAPITokensFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omnilsp.json")
	if err := os.WriteFile(path, []byte(`{"readApiTokens":["tok-a","tok-b"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config with readApiTokens: %v", err)
	}
	if want := []string{"tok-a", "tok-b"}; len(cfg.ReadAPITokens) != len(want) ||
		cfg.ReadAPITokens[0] != want[0] || cfg.ReadAPITokens[1] != want[1] {
		t.Fatalf("expected %v from file, got %v", want, cfg.ReadAPITokens)
	}
	set := cfg.ReadAPITokenSet()
	if !set["tok-a"] || !set["tok-b"] || len(set) != 2 {
		t.Fatalf("unexpected token set %v", set)
	}
}

func TestLoadReadAPITokensFromEnv(t *testing.T) {
	t.Setenv("OMNILSP_READ_API_TOKENS", "tok-a, tok-b ,,tok-c")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load config with read API token env: %v", err)
	}
	want := []string{"tok-a", "tok-b", "tok-c"}
	if len(cfg.ReadAPITokens) != len(want) {
		t.Fatalf("expected %v from env, got %v", want, cfg.ReadAPITokens)
	}
	for i, tok := range want {
		if cfg.ReadAPITokens[i] != tok {
			t.Fatalf("expected %v from env, got %v", want, cfg.ReadAPITokens)
		}
	}
}

// TestLoadReadAPITokensFileWinsOverEnv pins the Default < Env < File layering:
// a file key replaces the env-seeded token list entirely.
func TestLoadReadAPITokensFileWinsOverEnv(t *testing.T) {
	t.Setenv("OMNILSP_READ_API_TOKENS", "env-tok")
	path := filepath.Join(t.TempDir(), "omnilsp.json")
	if err := os.WriteFile(path, []byte(`{"readApiTokens":["file-tok"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.ReadAPITokens) != 1 || cfg.ReadAPITokens[0] != "file-tok" {
		t.Fatalf("file layer must win over env: got %v", cfg.ReadAPITokens)
	}
	if cfg.ReadAPITokenSet()["env-tok"] {
		t.Fatalf("env token survived the file layer: %v", cfg.ReadAPITokenSet())
	}
}

// TestLoadReadAPITokensEnvWithoutFile keeps env-only configuration working
// when the config file is absent (the serve default path).
func TestLoadReadAPITokensEnvWithoutFile(t *testing.T) {
	t.Setenv("OMNILSP_READ_API_TOKENS", "env-tok")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.ReadAPITokens) != 1 || cfg.ReadAPITokens[0] != "env-tok" {
		t.Fatalf("expected env token, got %v", cfg.ReadAPITokens)
	}
}

func TestReadAPITokenSetDropsEmptyAndDuplicates(t *testing.T) {
	cfg := Config{ReadAPITokens: []string{"a", "", "a"}}
	set := cfg.ReadAPITokenSet()
	if len(set) != 1 || !set["a"] {
		t.Fatalf("expected single-entry set {a}, got %v", set)
	}
}
