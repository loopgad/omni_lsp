package budget

import "testing"

func TestQuotaEnvNameDerivation(t *testing.T) {
	cases := map[string]string{
		"clangd":        "OMNILSP_BACKEND_QUOTA_CLANGD_BYTES",
		"pyright":       "OMNILSP_BACKEND_QUOTA_PYRIGHT_BYTES",
		"tsserver":      "OMNILSP_BACKEND_QUOTA_TSSERVER_BYTES",
		"rust-analyzer": "OMNILSP_BACKEND_QUOTA_RUST_ANALYZER_BYTES",
		"golang-native": "OMNILSP_BACKEND_QUOTA_GOLANG_NATIVE_BYTES",
		"ccls-memo":     "OMNILSP_BACKEND_QUOTA_CCLS_MEMO_BYTES",
	}
	for owner, want := range cases {
		if got := QuotaEnvName(owner); got != want {
			t.Errorf("QuotaEnvName(%q) = %q, want %q", owner, got, want)
		}
	}
}

func TestParsePositiveBytes(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		if _, ok := ParsePositiveBytes("OMNILSP_TEST_UNSET_BUDGET"); ok {
			t.Fatal("unset variable must report not-configured")
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv("OMNILSP_TEST_BUDGET", "4096")
		value, ok := ParsePositiveBytes("OMNILSP_TEST_BUDGET")
		if !ok || value != 4096 {
			t.Fatalf("ParsePositiveBytes = (%d, %v), want (4096, true)", value, ok)
		}
	})
	t.Run("zero and negative are invalid", func(t *testing.T) {
		t.Setenv("OMNILSP_TEST_BUDGET", "0")
		if _, ok := ParsePositiveBytes("OMNILSP_TEST_BUDGET"); ok {
			t.Fatal("zero budget must be rejected, not treated as configured")
		}
		t.Setenv("OMNILSP_TEST_BUDGET", "-5")
		if _, ok := ParsePositiveBytes("OMNILSP_TEST_BUDGET"); ok {
			t.Fatal("negative budget must be rejected")
		}
	})
	t.Run("garbage announces and stays off", func(t *testing.T) {
		t.Setenv("OMNILSP_TEST_BUDGET", "abc")
		if _, ok := ParsePositiveBytes("OMNILSP_TEST_BUDGET"); ok {
			t.Fatal("garbage budget must be rejected")
		}
	})
}
