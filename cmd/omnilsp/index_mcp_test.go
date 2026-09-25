package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/config"
)

func TestMCPIndexStatusInitializesWorkspace(t *testing.T) {
	root := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "index")
	configPath := filepath.Join(t.TempDir(), "omnilsp.json")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Transport = "mcp"
	cfg.IndexDir = indexDir
	cfg.Backends = []config.BackendConfig{
		{LanguageID: "go", Enabled: false},
		{LanguageID: "c", Enabled: false},
		{LanguageID: "cpp", Enabled: false},
		{LanguageID: "rust", Enabled: false},
		{LanguageID: "python", Enabled: false},
		{LanguageID: "typescript", Enabled: false},
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNILSP_TRUST", "trusted")

	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		t.Fatal(err)
	}
	previousStdin, previousStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inRead, outWrite
	defer func() { os.Stdin, os.Stdout = previousStdin, previousStdout }()

	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"index_status"}}` + "\n"
	if _, err := io.WriteString(inWrite, request); err != nil {
		t.Fatal(err)
	}
	_ = inWrite.Close()

	cmdServe([]string{"--config", configPath, "--transport", "mcp", "--workspace", root})
	_ = outWrite.Close()
	os.Stdin, os.Stdout = previousStdin, previousStdout
	_ = inRead.Close()
	defer outRead.Close()
	output, err := io.ReadAll(outRead)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error  any `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("MCP response %q: %v", output, err)
	}
	if response.Error != nil || len(response.Result.Content) != 1 {
		t.Fatalf("MCP response = %+v", response)
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &stats); err != nil {
		t.Fatalf("index_status text %q: %v", response.Result.Content[0].Text, err)
	}
	if stats["enabled"] != true || stats["indexDir"] != indexDir {
		t.Fatalf("index_status = %+v", stats)
	}
}
