package budget

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment surface for the §F14 backend-buffer production wiring
// (DEF-BUDWIRE, F14 half). The F13 half is wired behind
// OMNILSP_INDEX_MEMORY_BUDGET_BYTES in internal/runtime/server; this package
// carries the F14 half's construction inputs so internal/languages (and the
// plugin lane) can build a Manager without importing server internals.
const (
	// EnvBackendBudgetBytes names the global hard limit in bytes for all
	// backend buffer accounting. Unset keeps the whole F14 wiring off (the
	// X10 red line: no configured budget, no behavior change).
	EnvBackendBudgetBytes = "OMNILSP_BACKEND_BUDGET_BYTES"

	// EnvBackendQuotaPrefix and EnvBackendQuotaSuffix delimit the per-owner
	// quota variables: OMNILSP_BACKEND_QUOTA_<OWNER>_BYTES, where <OWNER> is
	// the owner name upper-cased with every non-alphanumeric byte mapped to
	// '_'. A per-owner quota caps that owner independently of the global
	// limit (§F14 per-backend budgets).
	EnvBackendQuotaPrefix = "OMNILSP_BACKEND_QUOTA_"
	EnvBackendQuotaSuffix = "_BYTES"
)

// QuotaEnvName derives the per-owner quota environment variable for owner.
// The mapping is deterministic but not injective: "rust-analyzer" and
// "rust_analyzer" derive the same variable. The repo's owner set
// (clangd, pyright, tsserver, rust-analyzer, golang-native, ccls-memo) has no
// collision.
func QuotaEnvName(owner string) string {
	var b strings.Builder
	b.WriteString(EnvBackendQuotaPrefix)
	for i := 0; i < len(owner); i++ {
		c := owner[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString(EnvBackendQuotaSuffix)
	return b.String()
}

// ParsePositiveBytes reads name as a positive integer byte count. It returns
// (0, false) when the variable is unset, and announces invalid values on
// stderr while keeping the budget off instead of guessing — the same
// positiveIntFromEnv convention the F13 ladder uses (server/index.go
// newIndexBudgetLadder). Zero and negative are invalid: a non-positive budget
// would refuse every allocation.
func ParsePositiveBytes(name string) (int64, bool) {
	text := strings.TrimSpace(os.Getenv(name))
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		fmt.Fprintf(os.Stderr, "omnilsp: ignoring invalid %s=%q (want a positive integer); the budget stays off\n", name, text)
		return 0, false
	}
	return value, true
}

// EnvironQuotaLimits scans the process environment for per-owner quota
// variables and returns the configured ones as env-name → limit. This lets a
// latching construction point honor quotas for owners it does not know by
// name (arbitrary nested.Conn names, future plugin lanes) instead of
// silently dropping them to global-only accounting.
func EnvironQuotaLimits() map[string]int64 {
	out := map[string]int64{}
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" ||
			!strings.HasPrefix(name, EnvBackendQuotaPrefix) ||
			!strings.HasSuffix(name, EnvBackendQuotaSuffix) {
			continue
		}
		if limit, ok := ParsePositiveBytes(name); ok {
			out[name] = limit
		}
	}
	return out
}
