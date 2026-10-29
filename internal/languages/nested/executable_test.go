package nested

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableIdentityResolvesAndHashesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.exe")
	content := []byte("fixed executable bytes")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	gotPath, gotHash, err := ExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	if gotPath != path || gotHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("identity = (%q,%q), want (%q,%q)", gotPath, gotHash, path, hex.EncodeToString(wantHash[:]))
	}
}

func TestExecutableIdentityRejectsDirectory(t *testing.T) {
	if _, _, err := ExecutableIdentity(t.TempDir()); err == nil {
		t.Fatal("directory was accepted as an executable")
	}
}

// TestExecutableIdentityContextHonorsCancellation 钉住可取消身份哈希（审计批 1，
// X10 兜底）：全量 SHA-256 原先 io.Copy 无上限，accept 关键路径上的大文件哈希
// 不能把调用方钉死在无界 I/O 上——ctx 已取消时必须立刻中止并报错。
func TestExecutableIdentityContextHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.exe")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ExecutableIdentityContext(ctx, path); err == nil {
		t.Fatal("cancelled context must abort the identity hash")
	}
}

// TestExecutableIdentityContextMatchesLegacyHash 钉住两条路径的等价性：同一
// 文件经可取消变体与遗留包装产出的摘要必须逐字节一致——包装只是补一个
// 不可取消的 ctx，不得改变哈希本身。
func TestExecutableIdentityContextMatchesLegacyHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.exe")
	content := []byte("fixed executable bytes")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath, legacyHash, err := ExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	ctxPath, ctxHash, err := ExecutableIdentityContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if legacyPath != ctxPath || legacyHash != ctxHash {
		t.Fatalf("identity mismatch: legacy (%q,%q) vs ctx (%q,%q)", legacyPath, legacyHash, ctxPath, ctxHash)
	}
}
