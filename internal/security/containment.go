// Package security enforces workspace trust boundaries.
//
// Concurrency model: single-writer per connection; reads served from captured
// snapshots; no goroutine escapes the owning supervisor.
//
// Invariants:
//  1. Fail-closed semantics: missing inputs produce refusals, never guesses.
//  2. Wire messages are versioned surfaces; changes require schema bumps.
//  3. Errors carry typed identity per internal/errors conventions.
package security

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	syserr "github.com/omnilsp/omni/internal/errors"
)

const opResolve = "security.ResolveInWorkspace"

// ErrPathEscapesWorkspace 是 §N7 的类型化哨兵错误：目标的"有效文件系统路径"
// 解析后逃逸出 workspace 根目录。
//
// 遵循 internal/errors 分类法（Kind=ErrPermission；平台不变量 3 规定错误比较
// 按 Kind 而非字符串）。判定方式：errors.Is(err, ErrPathEscapesWorkspace)。
//
// 安全约束：错误文本固定，绝不携带解析出的绝对路径（防止通过错误消息泄漏
// workspace 外的文件系统布局，见 TestSEC_N7_SymlinkEscapeContained）。
var ErrPathEscapesWorkspace = syserr.New(syserr.ErrPermission, opResolve,
	"effective path resolves outside the workspace root")

// ResolveInWorkspace 将 target 解析为 root 内的有效文件系统路径并返回；
// 若有效路径（含逐组件符号链接求值）落在 root 之外，返回
// ErrPathEscapesWorkspace。
//
// §N7 要求"canonical target -> containment policy"，禁止仅做字符串前缀检查：
// 本实现对每个路径组件做 Lstat/Readlink，符号链接目标先规范化再校验包含性，
// 因此 link/../x 这类"词典清洗掩盖穿越"的经典绕过无法得逞。
//
// Windows 注意事项：包含性比较大小写不敏感（卷名与路径大小写随意），已归一。
func ResolveInWorkspace(root, target string) (string, error) {
	if root == "" {
		return "", syserr.New(syserr.ErrInvalidArgument, opResolve, "empty workspace root")
	}
	// 防御纵深：编码变体（%2e%2e 等）在触碰文件系统前直接拒绝。
	if IsTraversalCandidate(target) {
		return "", ErrPathEscapesWorkspace
	}
	rootCanon, err := canonicalPath(root)
	if err != nil {
		return "", syserr.Wrap(syserr.ErrInternal, opResolve,
			fmt.Errorf("canonicalize workspace root: %w", err))
	}

	var (
		resolved string
		rerr     error
	)
	if filepath.IsAbs(target) {
		// 绝对路径：整体规范化（EvalSymlinks 覆盖内部所有链接）后校验包含性。
		resolved, rerr = canonicalPath(target)
		if rerr != nil {
			return "", syserr.Wrap(syserr.ErrNotFound, opResolve, rerr)
		}
	} else {
		resolved, rerr = resolveFrom(rootCanon, target)
		if rerr != nil {
			return "", rerr
		}
	}
	// 兜底断言：任何分支的结果都必须仍在根内。
	if !contained(rootCanon, resolved) {
		return "", ErrPathEscapesWorkspace
	}
	return resolved, nil
}

// IsTraversalCandidate 报告 raw 是否携带父目录穿越意图，含 URL 编码变体
// （%2e%2e、%2f、%5c）。畸形百分号编码按敌意处理（fail closed）。
//
// ponytail: 从宽拒绝——任何含 ".." 段或百分号编码分隔符的输入一律视为候选，
// 误伤 "a..b.txt" / 字面 "%2f" 文件名的场景在此安全闸口可接受；需要放行时再收紧。
func IsTraversalCandidate(raw string) bool {
	if raw == "" {
		return false
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return true
	}
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return true // 畸形编码：拒绝
	}
	for _, seg := range strings.FieldsFunc(dec, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// resolveFrom 自 base（已规范化）出发逐组件行走 target，遇符号链接即解析其
// 目标并重新校验包含性；".." 只在已解析的真实路径上上跳，无法跨越链接边界
// 掩盖逃逸。末段组件允许不存在（供后续创建流程使用），中间组件缺失则报错。
func resolveFrom(base, target string) (string, error) {
	cur := base
	parts := splitComponents(target)
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			up := filepath.Dir(cur)
			if !contained(base, up) {
				return "", ErrPathEscapesWorkspace
			}
			cur = up
		default:
			next := filepath.Join(cur, part)
			fi, lerr := os.Lstat(next)
			switch {
			case lerr == nil && fi.Mode()&os.ModeSymlink != 0:
				dest, rerr := os.Readlink(next)
				if rerr != nil {
					return "", syserr.Wrap(syserr.ErrInternal, opResolve, rerr)
				}
				if !filepath.IsAbs(dest) {
					dest = filepath.Join(cur, dest)
				}
				destCanon, cerr := canonicalPath(dest)
				if cerr != nil {
					return "", syserr.Wrap(syserr.ErrNotFound, opResolve, cerr)
				}
				if !contained(base, destCanon) {
					return "", ErrPathEscapesWorkspace
				}
				cur = destCanon
			case lerr == nil:
				cur = next
			default:
				if i == len(parts)-1 {
					cur = next // 尾段不存在：其中不可能有符号链接，安全
				} else {
					return "", syserr.New(syserr.ErrNotFound, opResolve,
						"intermediate component does not exist")
				}
			}
		}
	}
	return cur, nil
}

// canonicalPath 将 p 绝对化并对"最长存在前缀"求值符号链接；不存在尾部不含
// 链接，原样拼回。
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	tail := ""
	cur := filepath.Clean(abs)
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("%w: %s", os.ErrNotExist, p)
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// contained 报告 p 是否为 root 自身或位于其下。Windows 上大小写不敏感
// （NTFS/patext 路径与盘符大小写随意），其余平台区分大小写。
//
// ponytail: 整串 ToLower 而非逐组件 EqualFold，Windows 路径语义下等价且更省。
func contained(root, p string) bool {
	rel, err := filepath.Rel(normCase(root), normCase(p))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func normCase(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// splitComponents 同时接受 '/' 与 '\\' 分隔（客户端常混用两种分隔符）。
func splitComponents(target string) []string {
	return strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == '\\' })
}
