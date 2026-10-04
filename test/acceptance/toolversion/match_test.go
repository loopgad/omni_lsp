package toolversion

import "testing"

func TestMatchesRequiresStandaloneLockedVersion(t *testing.T) {
	tests := []struct {
		name, actual, expected string
		want                   bool
	}{
		{"exact", "1.2.3", "1.2.3", true},
		{"verbose prefix and suffix", "go version go1.2.3 windows/amd64", "go1.2.3", true},
		{"adjacent patch digit", "tool 1.2.30", "1.2.3", false},
		{"adjacent alphabetic suffix", "clangd 22.1.5-extra", "22.1.5", false},
		{"missing actual", "", "1.2.3", false},
		{"missing expected", "tool 1.2.3", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Matches(test.actual, test.expected); got != test.want {
				t.Fatalf("Matches(%q, %q) = %t; want %t", test.actual, test.expected, got, test.want)
			}
		})
	}
}
