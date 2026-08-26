package config

import (
	"strings"
	"testing"
	"time"
)

func TestR7_FeatureFlagValidation(t *testing.T) {
	t.Run("known flag accepted", func(t *testing.T) {
		if err := ValidateFlags(map[string]bool{"watch.externalFiles": true}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown flag rejected with known list", func(t *testing.T) {
		err := ValidateFlags(map[string]bool{"watch.external": true})
		if err == nil || !strings.Contains(err.Error(), "unknown feature flag") || !strings.Contains(err.Error(), "watch.externalFiles") {
			t.Fatalf("expected unknown-flag error listing known names, got %v", err)
		}
	})

	t.Run("expired flag rejected", func(t *testing.T) {
		KnownFlags["test.expired"] = FlagMeta{Owner: "test", Expiry: time.Now().Add(-time.Hour)}
		defer delete(KnownFlags, "test.expired")
		err := ValidateFlags(map[string]bool{"test.expired": true})
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expected expiry rejection, got %v", err)
		}
	})
}
