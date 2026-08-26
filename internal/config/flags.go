package config

import (
	"fmt"
	"strings"
	"time"
)

// §R7 feature flags: a registry with ownership and expiry metadata. Flags are
// opt-in kill-switches for bounded experiments — not configuration. An
// expired flag is rejected at validation time so dead switches cannot rot in
// production configs.

type FlagMeta struct {
	Owner   string
	Expiry  time.Time // zero = no expiry (discouraged, reviewed quarterly)
	Purpose string
}

var KnownFlags = map[string]FlagMeta{
	"watch.externalFiles": {
		Owner:   "runtime",
		Purpose: "enables the §D14 workspace poller alongside client didChangeWatchedFiles",
	},
}

// ValidateFlags checks every configured flag against the registry and its
// expiry. Returns all problems at once so one config pass surfaces everything.
func ValidateFlags(flags map[string]bool) error {
	var probs []string
	for name := range flags {
		meta, ok := KnownFlags[name]
		if !ok {
			probs = append(probs, fmt.Sprintf("unknown feature flag %q (known: %s)", name, knownNames()))
			continue
		}
		if !meta.Expiry.IsZero() && time.Now().After(meta.Expiry) {
			probs = append(probs, fmt.Sprintf("feature flag %q expired %s (owner %s)", name, meta.Expiry.Format(time.DateOnly), meta.Owner))
		}
	}
	if len(probs) == 0 {
		return nil
	}
	return fmt.Errorf("feature flags: %s", strings.Join(probs, "; "))
}

func knownNames() string {
	names := make([]string, 0, len(KnownFlags))
	for n := range KnownFlags {
		names = append(names, n)
	}
	// deterministic for stable error messages
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}
