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
	LogLevel              string `json:"logLevel"`
	Transport             string `json:"transport"`
	TCPAddr               string `json:"tcpAddr,omitempty"`
	MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
	MaxQueueSize          int    `json:"maxQueueSize"`
	WorkspaceDir          string `json:"workspaceDir,omitempty"`
	// IndexDir selects the persistent index directory. An empty value lets the
	// server derive a per-workspace path outside the user's repository.
	IndexDir string `json:"indexDir,omitempty"`
	// IndexDiskBudgetBytes limits persistent index storage. Zero disables the
	// limit; the default is 256 MiB.
	IndexDiskBudgetBytes int64           `json:"indexDiskBudgetBytes"`
	Backends             []BackendConfig `json:"backends,omitempty"`
	RequestTimeoutMs     int             `json:"requestTimeoutMs"`
	// ReadAPITokens lists the bearer tokens accepted by the read-only HTTP
	// API (§X6/N8). The file layer is the `readApiTokens` JSON array; the
	// OMNILSP_READ_API_TOKENS environment variable (comma-separated) seeds
	// the same field at the env layer, so a file key wins per
	// Default < Env < File. Empty = local-trust allow-all (no auth wrapper).
	ReadAPITokens []string `json:"readApiTokens,omitempty"`
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
		IndexDiskBudgetBytes:  256 * 1024 * 1024,
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
	if v := os.Getenv("OMNILSP_INDEX_DIR"); v != "" {
		cfg.IndexDir = v
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
	if v := os.Getenv("OMNILSP_READ_API_TOKENS"); v != "" {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				cfg.ReadAPITokens = append(cfg.ReadAPITokens, tok)
			}
		}
	}
	return cfg
}

// ReadAPITokenSet returns the read-API bearer tokens in the set shape
// transport/httpserver's AuthOptions.Tokens expects: deduplicated, empty
// entries dropped. A nil/empty result keeps the local-trust allow-all
// posture (auth wrapper not installed).
func (c Config) ReadAPITokenSet() map[string]bool {
	if len(c.ReadAPITokens) == 0 {
		return nil
	}
	tokens := make(map[string]bool, len(c.ReadAPITokens))
	for _, tok := range c.ReadAPITokens {
		if tok != "" {
			tokens[tok] = true
		}
	}
	return tokens
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
	if c.IndexDiskBudgetBytes < 0 {
		return fmt.Errorf("config: IndexDiskBudgetBytes must be non-negative")
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
