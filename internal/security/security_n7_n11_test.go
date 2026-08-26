package security

import (
	stderrors "errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSEC_N7_SymlinkEscapeContained 验证 §N7：workspace 内指向外部目标的
// 符号链接（Windows 上可能退化为目录 junction）在解析时被拒绝，且错误文本
// 不泄漏外部目标的绝对路径。
//
// Windows 符号链接创建通常需要管理员或开发者模式权限；先尝试 os.Symlink，
// 失败后退化为无需特权的目录 junction（cmd /c mklink /J）；两者皆失败则跳过。
func TestSEC_N7_SymlinkEscapeContained(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	link := filepath.Join(root, "evil")
	mode := ""
	if err := os.Symlink(outside, link); err == nil {
		mode = "symlink"
	} else if runtime.GOOS == "windows" {
		// Junction 仅可用于目录；mklink /J 不需要特权。
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput()
		if jerr != nil {
			t.Skipf("Windows 符号链接权限限制：os.Symlink=%v；junction=%v (%s)", err, jerr, out)
		}
		mode = "junction"
	} else {
		t.Fatalf("os.Symlink: %v", err)
	}
	t.Logf("逃逸链接类型：%s", mode)

	for _, rel := range []string{"evil", filepath.Join("evil", "secret.txt")} {
		got, err := ResolveInWorkspace(root, rel)
		if !stderrors.Is(err, ErrPathEscapesWorkspace) {
			t.Fatalf("[mode=%s] ResolveInWorkspace(%q)：期望 ErrPathEscapesWorkspace，得到 got=%q err=%v", mode, rel, got, err)
		}
		if got != "" {
			t.Fatalf("[mode=%s] 逃逸时不得返回路径，得到 %q", mode, got)
		}
		msg := err.Error()
		for _, leak := range []string{outside, secret, root} {
			if strings.Contains(msg, leak) {
				t.Fatalf("[mode=%s] 错误文本泄漏绝对路径 %q：%s", mode, leak, msg)
			}
		}
	}

	// 阳性对照：根内普通路径必须正常解析（证明函数本身可用）。
	inFile := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(inFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write in-root file: %v", err)
	}
	got, err := ResolveInWorkspace(root, "ok.txt")
	if err != nil {
		t.Fatalf("根内路径解析失败：%v", err)
	}
	if !filepath.IsAbs(got) || !strings.HasSuffix(got, "ok.txt") {
		t.Fatalf("根内路径解析结果异常：%q", got)
	}
}

// TestSEC_N7_PathTraversalRejected 验证 §N7/§S23：明文与编码变体的路径穿越
// 必须被拒（IsTraversalCandidate 标记 + ResolveInWorkspace 拒绝）。
func TestSEC_N7_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"dotdot_slash", "../../etc/passwd"},
		{"encoded_pct2e", "%2e%2e%2f"},
		{"encoded_mixed", "..%2f..%2fetc%2fpasswd"},
		{"dotdot_backslash", `..\..\windows`},
		{"dotdot_double_slash", "..//..//windows"},
	}
	root := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsTraversalCandidate(tc.raw) {
				t.Fatalf("IsTraversalCandidate(%q) = false，应为 true", tc.raw)
			}
			got, err := ResolveInWorkspace(root, tc.raw)
			if !stderrors.Is(err, ErrPathEscapesWorkspace) {
				t.Fatalf("ResolveInWorkspace(%q)：期望 ErrPathEscapesWorkspace，得到 got=%q err=%v", tc.raw, got, err)
			}
		})
	}
	// 负性对照：普通名字不是穿越候选，可正常解析。
	if IsTraversalCandidate("normal.txt") {
		t.Fatal("IsTraversalCandidate(\"normal.txt\") = true，应为 false")
	}
	if _, err := ResolveInWorkspace(root, "normal.txt"); err != nil {
		t.Fatalf("普通相对路径应可解析：%v", err)
	}
}

// TestSEC_N11_RedactionAudit 验证 §N11/§S23：RedactString 表驱动审计——
// 脱敏后不含原密文、保留键名结构、含 [REDACTED] 标记。
func TestSEC_N11_RedactionAudit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		keep []string // 必须保留（键名结构）
		drop []string // 必须消失（原密文）
	}{
		{
			name: "query_token",
			in:   "token=abc123",
			keep: []string{"token", "[REDACTED]"},
			drop: []string{"abc123"},
		},
		{
			name: "auth_header_bearer",
			in:   "Authorization: Bearer xyz",
			keep: []string{"Authorization", "[REDACTED]"},
			drop: []string{"xyz"},
		},
		{
			name: "json_password",
			in:   `{"password": "hunter2"}`,
			keep: []string{"password", "[REDACTED]"},
			drop: []string{"hunter2"},
		},
		{
			name: "api_key_dash",
			in:   "api-key: sk-9876543210",
			keep: []string{"api-key", "[REDACTED]"},
			drop: []string{"sk-9876543210"},
		},
		{
			name: "multiple_secrets",
			in:   "GET /api?token=aaa&secret=bbb HTTP/1.1",
			keep: []string{"token", "secret", "[REDACTED]"},
			drop: []string{"aaa", "bbb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactString(tc.in)
			for _, k := range tc.keep {
				if !strings.Contains(out, k) {
					t.Fatalf("输出缺少应保留的键名结构 %q：输入 %q → 输出 %q", k, tc.in, out)
				}
			}
			for _, d := range tc.drop {
				if strings.Contains(out, d) {
					t.Fatalf("输出仍包含密文 %q：输入 %q → 输出 %q", d, tc.in, out)
				}
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Fatalf("输出缺少 [REDACTED] 标记：%q", out)
			}
		})
	}
}
