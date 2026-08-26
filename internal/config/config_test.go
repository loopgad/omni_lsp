package config

import (
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
