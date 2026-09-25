package config

import "testing"

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
