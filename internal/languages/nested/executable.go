package nested

import (
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
func ExecutableIdentity(path string) (string, string, error) {
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
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	return abs, hex.EncodeToString(h.Sum(nil)), nil
}
