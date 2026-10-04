package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRustSemanticIndexToolConfigPinsToolchainExecutables(t *testing.T) {
	if _, err := exec.LookPath("rustup"); err != nil {
		t.Skip("rustup is not installed")
	}
	config, err := rustSemanticIndexToolConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Toolchain == "" || config.Build.Options["procMacros"] != "true" || config.Build.Options["buildScripts"] != "true" {
		t.Fatalf("Rust semantic tool config omitted toolchain/build identity: %+v", config)
	}
	want := map[string]bool{"cargo": false, "rustc": false, "proc-macro-srv": false}
	for _, tool := range config.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if !filepath.IsAbs(tool.Path) || len(tool.SHA256) != 64 || tool.Version == "" {
			t.Errorf("tool is not fully pinned: %+v", tool)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing pinned %s identity", name)
		}
	}
}
