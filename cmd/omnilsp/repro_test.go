package main

// §N13/§P10 repro bundle 测试：隐私默认档、full-source 显式 opt-in、
// 无凭据泄漏、manifest schema。

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/security"
)

const reproSecret = "abc123"

const reproSrcFixture = "package x\n\nconst token = \"" + reproSecret + "\"\n"

// reproFixture 建一个含凭据键的配置、一个含凭据字面量的源文件和一个 trace。
func reproFixture(t *testing.T) (configPath, workspace, tracePath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "omnilsp.json")
	cfg := `{"logLevel":"info","transport":"stdio","maxConcurrentRequests":8,` +
		`"maxQueueSize":16,"requestTimeoutMs":1000,"token":"` + reproSecret + `"}`
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace = filepath.Join(dir, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "hello.go"), []byte(reproSrcFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	tracePath = filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(tracePath,
		[]byte("{\"seq\":0,\"dir\":\"meta\",\"payload\":{\"formatVersion\":1}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, workspace, tracePath
}

// reproBundle 跑 cmdRepro 并把 zip 解成 name→bytes。
func reproBundle(t *testing.T, args ...string) map[string][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "bundle.zip")
	args = append([]string{"--out", out}, args...)
	if err := cmdRepro(args); err != nil {
		t.Fatalf("cmdRepro(%v): %v", args, err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()
	entries := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		entries[f.Name] = b
	}
	return entries
}

func parseManifest(t *testing.T, entries map[string][]byte) (schema, mode string, contents []map[string]any) {
	t.Helper()
	raw, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("bundle missing manifest.json")
	}
	var m struct {
		Schema   string           `json:"schema"`
		Mode     string           `json:"mode"`
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest: %v\n%s", err, raw)
	}
	return m.Schema, m.Mode, m.Contents
}

func TestN13_DefaultModeIsRedacted(t *testing.T) {
	cfg, ws, trace := reproFixture(t)
	entries := reproBundle(t, "--config", cfg, "--workspace", ws, "--trace", trace)
	_, mode, _ := parseManifest(t, entries)
	if mode != string(ReproRedacted) {
		t.Errorf("default mode = %q, want %q", mode, ReproRedacted)
	}
}

func TestN13_FullSourceRequiresExplicitOptIn(t *testing.T) {
	cfg, ws, trace := reproFixture(t)
	for _, mode := range []string{string(ReproMetadataOnly), string(ReproRedacted)} {
		entries := reproBundle(t, "--mode", mode, "--config", cfg, "--workspace", ws, "--trace", trace)
		for name := range entries {
			if strings.HasPrefix(name, "sources/") {
				t.Errorf("mode %s: must not carry sources/, found %q", mode, name)
			}
		}
	}

	full := reproBundle(t, "--mode", string(ReproFullSource), "--config", cfg, "--workspace", ws, "--trace", trace)
	got, ok := full["sources/hello.go"]
	if !ok {
		t.Fatalf("full-source bundle missing sources/hello.go; have %v", keys(full))
	}
	if string(got) != reproSrcFixture {
		t.Errorf("sources/hello.go = %q, want %q", got, reproSrcFixture)
	}
}

func TestN13_NoSecretsInBundle(t *testing.T) {
	cfg, ws, trace := reproFixture(t)
	for _, mode := range []string{string(ReproMetadataOnly), string(ReproRedacted), string(ReproFullSource)} {
		entries := reproBundle(t, "--mode", mode, "--config", cfg, "--workspace", ws, "--trace", trace)
		var all strings.Builder
		for _, data := range entries {
			all.WriteString(security.RedactString(string(data)))
			all.WriteByte('\n')
		}
		if strings.Contains(all.String(), reproSecret) {
			t.Errorf("mode %s: secret %q leaked in bundle:\n%s", mode, reproSecret, all.String())
		}
	}
}

func TestP10_ManifestSchema(t *testing.T) {
	cfg, ws, trace := reproFixture(t)
	entries := reproBundle(t, "--mode", string(ReproMetadataOnly), "--config", cfg, "--workspace", ws, "--trace", trace)
	schema, _, contents := parseManifest(t, entries)
	if schema != "omnilsp.repro.v1" {
		t.Errorf("schema = %q, want omnilsp.repro.v1", schema)
	}
	if len(contents) == 0 {
		t.Error("manifest.contents is empty")
	}
	// metadata 模式：trace 只留存根，不复制内容
	if _, ok := entries["session.trace.meta.json"]; !ok {
		t.Error("metadata mode missing session.trace.meta.json stub")
	}
	if _, ok := entries["session.trace.jsonl"]; ok {
		t.Error("metadata mode must not embed session.trace.jsonl")
	}
}

func TestP10_ManifestTimestampRFC3339(t *testing.T) {
	cfg, ws, _ := reproFixture(t)
	entries := reproBundle(t, "--config", cfg, "--workspace", ws)
	var m struct {
		CreatedAt string `json:"createdAtRFC3339"`
	}
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		t.Errorf("createdAtRFC3339 %q not RFC3339: %v", m.CreatedAt, err)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
