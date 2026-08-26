package config

import (
	"testing"
)

// FuzzConfigLoad validates that arbitrary string inputs to Load never panic.
func FuzzConfigLoad(f *testing.F) {
	seeds := []string{
		"",
		"/etc/omnilsp.json",
		"C:/Program Files/OmniLSP/config.json",
		"//nonexistent/path",
		`\x00\x01\x02`,
		"///////",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		// Load should never panic regardless of path content.
		_, _ = Load(path)
	})
}

// FuzzConfigValidate exercises Validate with adversarial config values.
func FuzzConfigValidate(f *testing.F) {
	f.Add("stdio", 64, 1024, "info")
	f.Add("tcp", 1, 1, "debug")
	f.Add("invalid", -1, -1, "error")
	f.Add("ipc", 0, 0, "warn")

	f.Fuzz(func(t *testing.T, transport string, maxReq, maxQueue int, logLevel string) {
		cfg := Config{
			LogLevel:              logLevel,
			Transport:             transport,
			MaxConcurrentRequests: maxReq,
			MaxQueueSize:          maxQueue,
			Backends: []BackendConfig{
				{LanguageID: "", Enabled: true},
				{LanguageID: "go", Enabled: false},
				{LanguageID: `\x00\x01`, Enabled: true},
			},
		}
		// Validate should never panic.
		_ = cfg.Validate()
	})
}
