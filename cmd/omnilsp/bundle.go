package main

// omnilsp repro bundle — goal.md §P10（复现包）+ §N13（隐私分级）。
//
// 包体是单个 .zip，内容随隐私模式变化：
//
//	metadata      仅摘要；trace 只记 sha256+大小存根
//	redacted      （默认）+ 脱敏配置快照 + 源文件清单（不含内容）
//	full-source   显式 opt-in 后额外逐字复制工作区源码
//
// 任何模式：不写入环境变量原文；token/password 等凭据键一律经
// internal/security.RedactString 脱敏（§N11）。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/security"
)

// PrivacyMode 选择复现包携带的素材层级（§N13）。
type PrivacyMode string

const (
	ReproMetadataOnly PrivacyMode = "metadata"
	ReproRedacted     PrivacyMode = "redacted" // 默认
	ReproFullSource   PrivacyMode = "full-source"
)

const reproSchema = "omnilsp.repro.v1"

// BundleOptions 描述一次复现包构建请求。
type BundleOptions struct {
	Mode       PrivacyMode
	Workspace  string        // full-source 模式下逐字复制该目录
	TracePath  string        // 可选 §P9 会话录制文件（.jsonl）
	Out        string        // 输出 .zip；空 → repro-<timestamp>.zip
	ConfigPath string        // 原始 omnilsp.json（优先作为快照来源）
	Config     config.Config // 解析后的配置（回退快照 + backend 清单）
}

type bundleEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type reproManifest struct {
	Schema      string        `json:"schema"`
	Mode        PrivacyMode   `json:"mode"`
	CreatedAt   string        `json:"createdAtRFC3339"`
	ToolVersion string        `json:"toolVersion"`
	Meta        replay.Meta   `json:"meta"` // 复用 §P9 头记录结构
	Contents    []bundleEntry `json:"contents"`
}

// Build 组装复现包并返回其落盘路径。
func Build(ctx context.Context, opts BundleOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch opts.Mode {
	case ReproMetadataOnly, ReproRedacted, ReproFullSource:
	default:
		return "", fmt.Errorf("repro: unknown privacy mode %q (want metadata|redacted|full-source)", opts.Mode)
	}
	if opts.Mode == ReproFullSource && opts.Workspace == "" {
		return "", fmt.Errorf("repro: --mode full-source requires --workspace")
	}

	out := opts.Out
	if out == "" {
		out = "repro-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	}

	entries := []bundleEntry{}
	payload := map[string][]byte{}
	add := func(name string, data []byte) {
		sum := sha256.Sum256(data)
		entries = append(entries, bundleEntry{
			Name: name, SHA256: hex.EncodeToString(sum[:]), Bytes: len(data),
		})
		payload[name] = data
	}

	add("environment.txt", environmentText(opts.Config))
	// 凭据键在任何模式下都必须脱敏，RedactString 全文处理。
	add("config.snapshot.json", configSnapshot(opts))
	backendsJSON, _ := json.MarshalIndent(opts.Config.Backends, "", "  ")
	add("backends.json", backendsJSON)

	if opts.TracePath != "" {
		data, err := os.ReadFile(opts.TracePath)
		if err != nil {
			return "", fmt.Errorf("repro: trace: %w", err)
		}
		if opts.Mode == ReproMetadataOnly {
			sum := sha256.Sum256(data)
			stub, _ := json.MarshalIndent(struct {
				SHA256 string `json:"sha256"`
				Bytes  int    `json:"bytes"`
			}{SHA256: hex.EncodeToString(sum[:]), Bytes: len(data)}, "", "  ")
			add("session.trace.meta.json", stub)
		} else {
			add("session.trace.jsonl", data)
		}
	}

	if opts.Mode == ReproRedacted {
		listing, err := redactedSourceListing(ctx, opts.Workspace)
		if err != nil {
			return "", fmt.Errorf("repro: sources: %w", err)
		}
		add("sources.redacted.json", listing)
	}
	if opts.Mode == ReproFullSource {
		srcs, err := collectSources(ctx, opts.Workspace)
		if err != nil {
			return "", fmt.Errorf("repro: sources: %w", err)
		}
		for _, s := range srcs {
			add("sources/"+s.name, s.data)
		}
	}

	man := reproManifest{
		Schema:      reproSchema,
		Mode:        opts.Mode,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		ToolVersion: version,
		Meta: replay.Meta{
			ConfigHash:   replay.ConfigHash(opts.Config),
			Backends:     backendInventory(opts.Config),
			Toolchain:    map[string]string{"go": runtime.Version()},
			WorkspaceDir: opts.Workspace,
		},
		Contents: entries,
	}
	manJSON, _ := json.MarshalIndent(man, "", "  ")

	if err := writeZip(out, manJSON, payload); err != nil {
		return "", err
	}
	return out, nil
}

func writeZip(path string, manifestJSON []byte, payload map[string][]byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("repro: create %q: %w", path, err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	write := func(name string, data []byte) error {
		fw, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := write("manifest.json", manifestJSON); err != nil {
		return err
	}
	names := make([]string, 0, len(payload))
	for n := range payload {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := write(n, payload[n]); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("repro: zip: %w", err)
	}
	return f.Close()
}

// environmentText 是 §P10 的环境 digest：GOOS/GOARCH/GOROOT/Go 运行时版本、
// 工具版本与配置哈希。刻意不包含 os.Environ() 原文。
func environmentText(cfg config.Config) []byte {
	return []byte(fmt.Sprintf(
		"GOOS=%s\nGOARCH=%s\nGOROOT=%s\ngo=%s\nomnilsp=%s\nconfigHash=%s\n",
		runtime.GOOS, runtime.GOARCH, runtime.GOROOT(), runtime.Version(),
		version, replay.ConfigHash(cfg)))
}

// configSnapshot 优先保留原始配置文件字节（未知键如 token 才能留存并被
// 脱敏），无文件时回退为解析后的 Config 序列化；两种来源都过 RedactString。
func configSnapshot(opts BundleOptions) []byte {
	raw, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		b, merr := json.MarshalIndent(opts.Config, "", "  ")
		if merr != nil {
			b = []byte("{}")
		}
		raw = b
	}
	return []byte(security.RedactString(string(raw)))
}

func backendInventory(cfg config.Config) map[string]string {
	inv := map[string]string{}
	for _, b := range cfg.Backends {
		if b.Enabled {
			inv[b.LanguageID] = b.BinaryPath
		}
	}
	return inv
}

type sourceFile struct {
	name string // 相对 workspace 根、斜杠分隔
	data []byte
}

// ponytail: 条目整体驻内存；工作区极大时再改流式两遍扫描。
func collectSources(ctx context.Context, root string) ([]sourceFile, error) {
	var out []sourceFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, sourceFile{name: filepath.ToSlash(rel), data: data})
		return nil
	})
	return out, err
}

type sourceSummary struct {
	Path   string `json:"path"`
	Lines  int    `json:"lines"`
	SHA256 string `json:"sha256"`
}

// redactedSourceListing 只写路径+行数+sha256，绝不写内容（§N13 redacted 层）。
func redactedSourceListing(ctx context.Context, root string) ([]byte, error) {
	summaries := []sourceSummary{}
	if root == "" {
		return json.MarshalIndent(summaries, "", "  ")
	}
	srcs, err := collectSources(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, s := range srcs {
		sum := sha256.Sum256(s.data)
		summaries = append(summaries, sourceSummary{
			Path:   s.name,
			Lines:  countLines(s.data),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return json.MarshalIndent(summaries, "", "  ")
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
