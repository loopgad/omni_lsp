// Command acceptance-tools is built under three different Windows executable
// names so Go's os/exec can launch pinned npm language servers without relying
// on npm's .cmd shell shims.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

func run() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve wrapper executable: %w", err)
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(self)), filepath.Ext(self))
	toolRoot := filepath.Dir(filepath.Dir(self))
	var script string
	switch name {
	case "pyright-langserver":
		script = filepath.Join(toolRoot, "node_modules", "pyright", "langserver.index.js")
	case "typescript-language-server":
		script = filepath.Join(toolRoot, "node_modules", "typescript-language-server", "lib", "cli.mjs")
	case "tsc":
		script = filepath.Join(toolRoot, "node_modules", "typescript", "bin", "tsc")
	default:
		return fmt.Errorf("unsupported acceptance tool wrapper name %q", name)
	}
	if name == "pyright-langserver" && len(os.Args) == 2 && os.Args[1] == "--version" {
		var metadata struct {
			Version string `json:"version"`
		}
		packagePath := filepath.Join(toolRoot, "node_modules", "pyright", "package.json")
		data, err := os.ReadFile(packagePath)
		if err != nil {
			return fmt.Errorf("read pinned Pyright package metadata: %w", err)
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return fmt.Errorf("decode pinned Pyright package metadata: %w", err)
		}
		if metadata.Version == "" {
			return errors.New("pinned Pyright package metadata has no version")
		}
		fmt.Fprintf(os.Stdout, "Pyright %s\n", metadata.Version)
		return nil
	}
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("pinned %s entrypoint unavailable at %s: %w", name, script, err)
	}
	node, err := lockedNodePath()
	if err != nil {
		return fmt.Errorf("Node.js is required for pinned %s: %w", name, err)
	}
	args := append([]string{script}, os.Args[1:]...)
	cmd := exec.Command(node, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir, err = os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	return runCommandWithJob(cmd)
}

func lockedNodePath() (string, error) {
	configured := strings.TrimSpace(os.Getenv("OMNILSP_ACCEPTANCE_NODE"))
	if configured == "" {
		if os.Getenv("OMNILSP_ACCEPTANCE_LOCKED_TOOLS") == "1" {
			return "", errors.New("OMNILSP_ACCEPTANCE_NODE is required for locked acceptance execution")
		}
		return exec.LookPath("node")
	}
	if !filepath.IsAbs(configured) {
		return "", fmt.Errorf("OMNILSP_ACCEPTANCE_NODE must be absolute: %q", configured)
	}
	configured, err := filepath.Abs(filepath.Clean(configured))
	if err != nil {
		return "", fmt.Errorf("resolve OMNILSP_ACCEPTANCE_NODE: %w", err)
	}
	info, err := os.Stat(configured)
	if err != nil {
		return "", fmt.Errorf("locked Node executable is unavailable at %q: %w", configured, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("locked Node executable is a directory: %q", configured)
	}
	expected := strings.TrimSpace(os.Getenv("OMNILSP_ACCEPTANCE_NODE_SHA256"))
	if len(expected) != sha256.Size*2 {
		return "", errors.New("OMNILSP_ACCEPTANCE_NODE_SHA256 is required for locked acceptance execution")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", fmt.Errorf("OMNILSP_ACCEPTANCE_NODE_SHA256 is invalid: %w", err)
	}
	file, err := os.Open(configured)
	if err != nil {
		return "", fmt.Errorf("open locked Node executable: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("hash locked Node executable: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close locked Node executable: %w", closeErr)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return "", fmt.Errorf("locked Node executable SHA-256 is %s; lock requires %s", actual, strings.ToLower(expected))
	}
	return configured, nil
}
