// Package buildctx implements the canonical Build Context model for OmniLSP.
//
// Responsibility:
//
//	Derives digest-backed BuildContextIDs from canonicalized semantic inputs
//	per goal.md §E0/§E1/§E4/§E5. Semantic identity is:
//
//	    Source + Toolchain + Build Configuration + Dependency State + Env Inputs
//
//	A file path alone is not enough to identify program meaning.
//
// Owned mutable state:
//
//	None. Context is an immutable value type; ID() is deterministic.
//
// Concurrency model:
//
//	Immutable after construction; safe for concurrent use.
//
// Invariants:
//  1. E0: IDs derive from canonicalized semantic inputs, never wall-clock time.
//  2. E4: only the semantic environment allowlist is hashed — the full process
//     environment is NEVER hashed blindly.
//  3. E4: unordered maps are sorted before serialization; ordered slices keep
//     their order where it is semantically meaningful.
package buildctx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
)

// Provenance records how a build context was established (§E1).
type Provenance uint8

const (
	ProvenanceExplicitUserConfig Provenance = iota
	ProvenanceCanonicalBuildDB
	ProvenanceBuildSystemMetadata
	ProvenanceBackendDerived
	ProvenanceHeuristic
)

func (p Provenance) String() string {
	switch p {
	case ProvenanceExplicitUserConfig:
		return "explicit_user_config"
	case ProvenanceCanonicalBuildDB:
		return "canonical_build_db"
	case ProvenanceBuildSystemMetadata:
		return "build_system_metadata"
	case ProvenanceBackendDerived:
		return "backend_derived"
	case ProvenanceHeuristic:
		return "heuristic"
	default:
		return "unknown"
	}
}

// ToolchainIdentity identifies the compiler/language-service producing
// semantics (§E5). A changed toolchain MUST invalidate affected caches.
type ToolchainIdentity struct {
	Kind    string // e.g. "go", "clang"
	Version string // e.g. go1.26.1
	Target  string // e.g. GOOS/GOARCH or target triple
}

// Context is an immutable build context (§B2 subset required for v1 keys).
type Context struct {
	Language   string
	Toolchain  ToolchainIdentity
	WorkingDir string // canonical path
	Args       []string
	Defines    map[string]string
	Env        map[string]string // allowlisted entries only (§E4)
	Extra      map[string]string // language-specific sorted entries
	Provenance Provenance
}

// Canonicalize serializes the context deterministically: fields in fixed
// order, map keys sorted, slices preserved where ordering is semantic.
func (c Context) Canonicalize() []byte {
	var b strings.Builder
	writeField := func(k, v string) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(v)
		b.WriteByte(0)
	}
	writeField("lang", c.Language)
	writeField("tool.kind", c.Toolchain.Kind)
	writeField("tool.version", c.Toolchain.Version)
	writeField("tool.target", c.Toolchain.Target)
	writeField("dir", c.WorkingDir)
	for _, a := range c.Args { // slice: order preserved (semantic)
		writeField("arg", a)
	}
	writeSortedMap(&b, "define", c.Defines)
	writeSortedMap(&b, "env", c.Env)
	writeSortedMap(&b, "extra", c.Extra)
	writeField("prov", c.Provenance.String())
	return []byte(b.String())
}

// ID returns the digest-backed BuildContextID (§B2).
func (c Context) ID() identity.BuildContextID {
	sum := sha256.Sum256(c.Canonicalize())
	return identity.BuildContextID(c.Language + ":sha256:" + hex.EncodeToString(sum[:16]))
}

func writeSortedMap(b *strings.Builder, prefix string, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(prefix)
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(m[k])
		b.WriteByte(0)
	}
}

// GoEnvAllowlist lists the semantic environment variables that participate in
// Go build-context identity (§E7). Everything else is ignored by design.
var GoEnvAllowlist = []string{"GOOS", "GOARCH", "GOFLAGS", "GOWORK"}

// DeriveGo builds a Go build context from explicit inputs. It is a pure
// function so callers can cache the expensive `go env` probe themselves and
// tests can inject values without spawning processes.
func DeriveGo(workDir, goVersion, target string, env map[string]string, extraDefines map[string]string) (Context, error) {
	if workDir == "" {
		return Context{}, fmt.Errorf("buildctx: empty working dir")
	}
	filtered := make(map[string]string, len(env))
	for _, key := range GoEnvAllowlist {
		if v, ok := env[key]; ok && v != "" {
			filtered[key] = v
		}
	}
	return Context{
		Language:   "go",
		Toolchain:  ToolchainIdentity{Kind: "go", Version: goVersion, Target: target},
		WorkingDir: workDir,
		Env:        filtered,
		Defines:    extraDefines,
		Extra: map[string]string{
			"vendor.mode": env["GOFLAGS"], // vendor state rides GOFLAGS per §E7
		},
		Provenance: ProvenanceBackendDerived,
	}, nil
}
