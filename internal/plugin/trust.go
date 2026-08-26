package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrChecksumMismatch 表示 entrypoint 实际内容与声明的 sha256 不一致（N14）。
var ErrChecksumMismatch = errors.New("plugin: checksum 不匹配")

// Load 读取并反序列化 dir/plugin.json。未知字段一律拒绝，
// 防止敌意 manifest 携带未定义语义。
func Load(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: 读取 plugin.json: %w", err)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("plugin: 解析 plugin.json: %w", err)
	}
	return m, nil
}

// Verify 执行完整信任链：Load → Validate → entrypoint 路径安全检查 →
// 存在性检查 → sha256 比对。任一步失败即整体拒绝（N14：未验证元数据不可信）。
func Verify(dir string) (Manifest, error) {
	m, err := Load(dir)
	if err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	// ponytail: entrypoint 仅允许裸文件名（与 plugin.json 同级），
	// 覆盖穿越/绝对路径/盘符三类敌意样本；支持子目录需加相对路径遍历校验。
	ep := m.Entrypoint
	if ep != filepath.Base(ep) || strings.Contains(ep, "..") ||
		filepath.IsAbs(ep) || strings.ContainsRune(ep, ':') {
		return m, fmt.Errorf("plugin: 敌意 entrypoint 被拒绝: %q", ep)
	}
	p := filepath.Join(dir, ep)
	if _, err := os.Stat(p); err != nil {
		return m, fmt.Errorf("plugin: entrypoint 缺失: %w", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return m, fmt.Errorf("plugin: 打开 entrypoint: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return Manifest{}, fmt.Errorf("plugin: 计算 sha256: %w", err)
	}
	actual := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	if actual != m.Checksum {
		return m, fmt.Errorf("%w: 声明 %s 实际 %s", ErrChecksumMismatch, m.Checksum, actual)
	}
	return m, nil
}
