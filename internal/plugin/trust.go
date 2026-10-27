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
	if err := safeEntrypoint(m.Entrypoint); err != nil {
		return m, err
	}
	p := filepath.Join(dir, m.Entrypoint)
	if _, err := os.Stat(p); err != nil {
		return m, fmt.Errorf("plugin: entrypoint 缺失: %w", err)
	}
	if err := verifyChecksum(p, m.Checksum); err != nil {
		return m, err
	}
	return m, nil
}

// safeEntrypoint 强制 entrypoint 为与 plugin.json 同级的裸文件名，覆盖
// 穿越/绝对路径/盘符三类敌意样本（ponytail：支持子目录需加相对路径遍历
// 校验）。Verify 与 Launch 共用——一旦目录来源进入执行路径，形态检查
// 必须在启动时刻同样生效，否则 Join(dir, ep) 可被带分隔符的 ep 引出目录。
func safeEntrypoint(ep string) error {
	if ep != filepath.Base(ep) || strings.Contains(ep, "..") ||
		filepath.IsAbs(ep) || strings.ContainsRune(ep, ':') {
		return fmt.Errorf("plugin: 敌意 entrypoint 被拒绝: %q", ep)
	}
	return nil
}

// verifyChecksum 计算 path 处文件的 sha256 并与 manifest 声明值比对（N14）。
func verifyChecksum(path, declared string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("plugin: 打开 entrypoint: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("plugin: 计算 sha256: %w", err)
	}
	actual := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	if actual != declared {
		return fmt.Errorf("%w: 声明 %s 实际 %s", ErrChecksumMismatch, declared, actual)
	}
	return nil
}
