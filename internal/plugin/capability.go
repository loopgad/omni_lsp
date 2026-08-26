package plugin

import "fmt"

// ErrCapabilityDenied 表示请求的能力未被授予（默认 deny，§O2）。
var ErrCapabilityDenied = fmt.Errorf("plugin: 能力被拒绝")

// Grant 是一张能力授予表。零值与 DefaultGrants() 等价于全 deny。
type Grant struct {
	caps map[Capability]bool
}

// DefaultGrants 返回全 deny 授权表（§O2：Default is deny）。
func DefaultGrants() Grant { return Grant{} }

// NewGrant 构造显式授权表，仅列出的能力放行。
func NewGrant(caps ...Capability) Grant {
	g := Grant{caps: make(map[Capability]bool, len(caps))}
	for _, c := range caps {
		g.caps[c] = true
	}
	return g
}

// Check 检查单项能力；未授予返回 ErrCapabilityDenied。
func (g Grant) Check(c Capability) error {
	if !g.caps[c] {
		return fmt.Errorf("%w: %q", ErrCapabilityDenied, c)
	}
	return nil
}
