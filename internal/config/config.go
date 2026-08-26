// Package config provides the canonical configuration schema for OmniLSP.
//
// Responsibility:
//
//	Defines the Config struct, default values, file loading, and validation.
//	Configuration precedence: Default < Env < File < CLI (later layers win).
//
// Owned mutable state:
//
//	None. Config is an immutable value type after construction.
//
// Concurrency model:
//
//	Read-only after construction. Safe for concurrent access.
//
// Invariants:
//  1. Validate() MUST reject zero or negative MaxConcurrentRequests/MaxQueueSize.
//  2. Transport MUST be one of: stdio, tcp, ipc, mcp.
//  3. Enabled backends MUST have a non-empty LanguageID.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Layer int

const (
	LayerDefault Layer = iota
	LayerEnv
	LayerFile
	LayerCLI
)

type Config struct {
	LogLevel              string          `json:"logLevel"`
	Transport             string          `json:"transport"`
	TCPAddr               string          `json:"tcpAddr,omitempty"`
	MaxConcurrentRequests int             `json:"maxConcurrentRequests"`
	MaxQueueSize          int             `json:"maxQueueSize"`
	WorkspaceDir          string          `json:"workspaceDir,omitempty"`
	Backends              []BackendConfig `json:"backends,omitempty"`
	RequestTimeoutMs      int             `json:"requestTimeoutMs"`
	// FeatureFlags (§R7): every flag must be registered in KnownFlags with an
	// owner and expiry; unknown keys fail Validate so typos never silently
	// disable behavior.
	FeatureFlags map[string]bool `json:"featureFlags,omitempty"`
}

type BackendConfig struct {
	LanguageID         string   `json:"languageId"`
	Enabled            bool     `json:"enabled"`
	BinaryPath         string   `json:"binaryPath,omitempty"`
	Args               []string `json:"args,omitempty"`
	MaxRestartAttempts int      `json:"maxRestartAttempts,omitempty"`
}

func Default() Config {
	return Config{
		LogLevel:              "info",
		Transport:             "stdio",
		MaxConcurrentRequests: 64,
		MaxQueueSize:          1024,
		RequestTimeoutMs:      5000,
	}
}

// Load resolves configuration across layers: Default < Env < File (later
// layers win, per the package doc). CLI flags are applied by cmd/omnilsp on
// top of the returned value. A missing file is not an error — defaults win.
func Load(path string) (Config, error) {
	cfg := applyEnv(Default())
	if path == "" {
		return cfg, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return cfg, fmt.Errorf("config: resolve path %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("config: read %q: %w", abs, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %q: %w", abs, err)
	}
	return cfg, nil
}

// applyEnv overlays OMNILSP_* environment variables onto cfg (LayerEnv).
func applyEnv(cfg Config) Config {
	if v := os.Getenv("OMNILSP_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("OMNILSP_TRANSPORT"); v != "" {
		cfg.Transport = v
	}
	if v := os.Getenv("OMNILSP_TCP_ADDR"); v != "" {
		cfg.TCPAddr = v
	}
	if v := os.Getenv("OMNILSP_WORKSPACE"); v != "" {
		cfg.WorkspaceDir = v
	}
	if v := os.Getenv("OMNILSP_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrentRequests = n
		}
	}
	if v := os.Getenv("OMNILSP_MAX_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxQueueSize = n
		}
	}
	return cfg
}

func (c Config) Validate() error {
	switch c.Transport {
	case "stdio", "tcp", "ipc", "mcp":
	default:
		return fmt.Errorf("config: unsupported transport %q", c.Transport)
	}
	if c.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("config: MaxConcurrentRequests must be positive")
	}
	if c.MaxQueueSize <= 0 {
		return fmt.Errorf("config: MaxQueueSize must be positive")
	}
	for _, b := range c.Backends {
		if !b.Enabled {
			continue
		}
		if b.LanguageID == "" {
			return fmt.Errorf("config: backend language ID must not be empty")
		}
	}
	if err := ValidateFlags(c.FeatureFlags); err != nil {
		return err
	}
	if !isValidLogLevel(c.LogLevel) {
		return fmt.Errorf("config: invalid log level %q", c.LogLevel)
	}
	return nil
}

func isValidLogLevel(lv string) bool {
	switch strings.ToLower(lv) {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
