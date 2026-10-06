// Package uri implements the canonical URI engine for OmniLSP.
//
// Responsibility:
//
//	Parsing, normalizing, and converting file URIs per goal.md §D2/§D3.
//	A URI carries two spellings: the original display form (never corrupted)
//	and a canonical identity used for maps, caches, and comparisons.
//
// Owned mutable state:
//
//	None. URI is an immutable value type.
//
// Concurrency model:
//
//	Immutable after Parse; safe for concurrent use.
//
// Invariants:
//  1. D2: normalization preserves a stable canonical identity without
//     corrupting display spelling.
//  2. Canonical(Parse(Canonical(u))) == Canonical(u) — idempotence (§S2).
//  3. Path() is only valid for file-scheme URIs; anything else is a typed
//     error, never a best-effort guess.
package uri

import (
	"fmt"
	"net/url"
	"runtime"
	"strings"
)

// URI is a parsed document URI with distinct display and canonical forms.
type URI struct {
	original  string // display spelling as provided
	canonical string // normalized identity
	scheme    string // lowercase scheme
}

// Parse parses and validates a URI string.
func Parse(s string) (URI, error) {
	if s == "" {
		return URI{}, fmt.Errorf("uri: empty string")
	}
	u, err := url.Parse(s)
	if err != nil {
		return URI{}, fmt.Errorf("uri: parse %q: %w", s, err)
	}
	if u.Scheme == "" {
		return URI{}, fmt.Errorf("uri: missing scheme: %q", s)
	}
	canon := normalize(u)
	return URI{original: s, canonical: canon, scheme: strings.ToLower(u.Scheme)}, nil
}

// FromPath converts an OS path into a file URI.
// Windows drive letters ("C:\x"), UNC paths ("\\srv\share\x"), and POSIX
// paths ("/x") are all accepted.
func FromPath(path string) URI {
	p := strings.ReplaceAll(path, "\\", "/")
	switch {
	case len(p) >= 2 && p[1] == ':': // drive letter
		return mustFile("file:///" + normalizeDrive(p))
	case strings.HasPrefix(p, "//"): // UNC
		return mustFile("file:" + p) // url.String re-encodes as file://host/...
	default:
		return mustFile("file://" + p)
	}
}

func mustFile(s string) URI {
	u, err := Parse(s)
	if err != nil {
		// FromPath inputs are structurally valid by construction; the only
		// failure mode would be a control-character path, which we surface as an
		// opaque URI carrying the raw spelling rather than panicking. The scheme
		// stays "file", so IsFile reports true and Path fails when it reparses
		// the same string -- a caller never receives a guessed path, but a
		// control-character path still leaves behind a URI that is not
		// canonical and will not compare equal to its own normalized form.
		return URI{original: s, canonical: s, scheme: "file"}
	}
	return u
}

// Canonical returns the normalized identity of the URI. Two URIs denote the
// same document iff their Canonical forms are equal.
func (u URI) Canonical() string { return u.canonical }

// String returns the original display spelling.
func (u URI) String() string { return u.original }

// Scheme returns the lowercased scheme.
func (u URI) Scheme() string { return u.scheme }

// IsFile reports whether the URI targets the local filesystem.
func (u URI) IsFile() bool { return u.scheme == "file" }

// Path converts a file URI into an OS path for the current platform.
// Non-file schemes return a typed error (D2: never guess).
func (u URI) Path() (string, error) {
	if !u.IsFile() {
		return "", fmt.Errorf("uri: %q is not a file URI (scheme %q)", u.original, u.scheme)
	}
	parsed, err := url.Parse(u.canonical)
	if err != nil {
		return "", fmt.Errorf("uri: reparse %q: %w", u.canonical, err)
	}
	host := strings.ToLower(parsed.Hostname())
	path := parsed.Path // decoded
	if runtimeIsWindows() {
		if host != "" {
			// UNC: file://server/share/x -> \\server\share\x
			return "\\\\" + host + strings.ReplaceAll(path, "/", "\\"), nil
		}
		// Drive letter: /c/x -> C:\x
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = strings.ToUpper(path[1:2]) + path[2:]
		}
		return strings.ReplaceAll(path, "/", "\\"), nil
	}
	if host != "" && host != "localhost" {
		return "", fmt.Errorf("uri: remote host %q not supported on this platform", host)
	}
	return path, nil
}

// PathOr returns the OS path or the empty string on error. Convenience for
// contexts where callers have already validated the scheme.
func (u URI) PathOr() string {
	p, _ := u.Path()
	return p
}

func runtimeIsWindows() bool { return runtime.GOOS == "windows" }

// normalize builds the canonical spelling of a parsed URL:
//   - lowercase scheme
//   - Windows drive letters lowercased in the authority+path
//   - empty authority dropped for localhost-style file URIs
//   - percent-encoded unreserved path characters decoded
//   - everything else re-encoded minimally by url.URL.String
func normalize(u *url.URL) string {
	cp := *u
	cp.Scheme = strings.ToLower(u.Scheme)
	if cp.Scheme == "file" {
		if cp.Host != "" {
			// Lowercase drive-letter hosts (file:///C:/... parses Host="c:").
			cp.Host = normalizeDrive(cp.Host)
		}
		if cp.Path != "" {
			cp.Path = normalizeDriveInPath(cp.Path)
			cp.RawPath = normalizeEscapedUnreservedPath(normalizeDriveInRawPath(cp.RawPath, cp.Path))
		}
		if cp.Opaque != "" {
			cp.Opaque = normalizeDrive(cp.Opaque)
		}
	}
	return cp.String()
}

// normalizeDrive lowercases a leading drive letter ("C:" -> "c:", "C/" -> "c/").
func normalizeDrive(s string) string {
	if len(s) >= 2 && s[1] == ':' && s[0] >= 'A' && s[0] <= 'Z' {
		return string(s[0]+32) + s[1:]
	}
	return s
}

// normalizeDriveInPath handles "/C:/x" style paths produced by some clients.
func normalizeDriveInPath(p string) string {
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && p[1] >= 'A' && p[1] <= 'Z' {
		return "/" + string(p[1]+32) + p[2:]
	}
	return p
}

// normalizeDriveInRawPath canonicalizes only the Windows drive prefix in an
// escaped path. url.URL preserves RawPath when a client spells the drive colon
// as %3A; without normalizing this hint, c:/ and c%3A/ produce different
// canonical identities even though Path has already decoded them equally.
// Keep escapes in the remainder intact (notably %2F, which is not a path
// separator in the original URI spelling).
func normalizeDriveInRawPath(rawPath, path string) string {
	if rawPath == "" || len(path) < 3 || path[0] != '/' || path[2] != ':' ||
		!isASCIILetter(path[1]) || len(rawPath) < 3 || rawPath[0] != '/' ||
		!isASCIILetter(rawPath[1]) {
		return rawPath
	}
	colonEnd := 0
	switch {
	case rawPath[2] == ':':
		colonEnd = 3
	case len(rawPath) >= 5 && strings.EqualFold(rawPath[2:5], "%3a"):
		colonEnd = 5
	default:
		return rawPath
	}
	return "/" + string(toLowerASCII(rawPath[1])) + ":" + rawPath[colonEnd:]
}

// normalizeEscapedUnreservedPath decodes percent escapes for RFC 3986
// unreserved ASCII bytes and uppercases the hex digits for other escapes.
// Escaped reserved bytes (especially %2F) stay escaped to preserve identity.
func normalizeEscapedUnreservedPath(rawPath string) string {
	if rawPath == "" || !strings.Contains(rawPath, "%") {
		return rawPath
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(rawPath))
	for i := 0; i < len(rawPath); i++ {
		if rawPath[i] != '%' || i+2 >= len(rawPath) {
			b.WriteByte(rawPath[i])
			continue
		}
		hi, okHi := hexDigit(rawPath[i+1])
		lo, okLo := hexDigit(rawPath[i+2])
		if !okHi || !okLo {
			b.WriteByte(rawPath[i])
			continue
		}
		value := hi<<4 | lo
		if isUnreservedASCII(value) {
			b.WriteByte(value)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[value>>4])
			b.WriteByte(hex[value&0x0f])
		}
		i += 2
	}
	return b.String()
}

func hexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func isUnreservedASCII(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~'
}

func isASCIILetter(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func toLowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
