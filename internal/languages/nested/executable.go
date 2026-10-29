package nested

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// ExecutablePath returns the resolved path of the currently attached child.
// It lets optional offline index extractors bind their tool identity to the
// exact executable used by the live backend without performing another PATH
// lookup. Empty means no process is attached.
func (c *Conn) ExecutablePath() string {
	c.mu.Lock()
	cmd := c.cmd
	path := ""
	if cmd != nil {
		path = cmd.Path
	}
	c.mu.Unlock()
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return filepath.Clean(path)
}

// ExecutableIdentity captures the absolute path and content hash for
// persistent semantic provenance. Callers should persist the returned path
// and digest; they must not re-resolve a floating command name during extraction.
//
// 遗留签名：等价于以不可取消的 context 调用 ExecutableIdentityContext，既有
// 调用方行为不变。accept 关键路径上的 pinning 链应改用可取消变体（审计批 1）。
func ExecutableIdentity(path string) (string, string, error) {
	return ExecutableIdentityContext(context.Background(), path)
}

// ExecutableIdentityContext is the cancelable variant of ExecutableIdentity:
// the full-file SHA-256 streams in bounded chunks and responds to ctx between
// reads, so a huge binary hash on the accept-loop critical path cannot pin the
// caller to unbounded I/O (audit batch 1 — the old io.Copy had no bound at
// all). The digest is identical to the legacy wrapper's for the same file.
func ExecutableIdentityContext(ctx context.Context, path string) (string, string, error) {
	if path == "" {
		return "", "", os.ErrInvalid
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	} else {
		return "", "", resolveErr
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", os.ErrInvalid
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if cerr := ctx.Err(); cerr != nil {
				_ = f.Close()
				return "", "", cerr
			}
			if _, herr := h.Write(buf[:n]); herr != nil {
				_ = f.Close()
				return "", "", herr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			return "", "", rerr
		}
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	return abs, hex.EncodeToString(h.Sum(nil)), nil
}
