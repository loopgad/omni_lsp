// Package toolversion contains exact version-lock checks shared by acceptance
// suites that parse different tool-specific version output formats.
package toolversion

import (
	"strings"
)

// Matches reports whether actual contains the complete locked version as a
// standalone token. Tool output may include a product prefix or platform
// suffix, but a longer adjacent version such as 1.2.30 cannot match 1.2.3.
func Matches(actual, expected string) bool {
	if strings.TrimSpace(actual) == "" || strings.TrimSpace(expected) == "" {
		return false
	}
	for offset := 0; offset < len(actual); {
		index := strings.Index(actual[offset:], expected)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(expected)
		leftOK := index == 0 || !isVersionCharacter(actual[index-1])
		// Some tools prefix a bare semver with "v" (for example, Neovim).
		if !leftOK && actual[index-1] == 'v' && index >= 2 && !isVersionCharacter(actual[index-2]) {
			leftOK = true
		}
		rightOK := end == len(actual) || !isVersionCharacter(actual[end])
		if leftOK && rightOK {
			return true
		}
		offset = index + 1
	}
	return false
}

func isVersionCharacter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '.' || value == '-' || value == '+' || value == '_'
}
