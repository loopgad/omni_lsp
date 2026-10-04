// Package replay records and replays LSP protocol sessions (goal.md §P9/P10).
//
// A session is a JSONL file: line 1 is a Meta record carrying the P9
// environment digests (config hash, backend versions, toolchain identity);
// every following line is one protocol message with its direction ("in" =
// client→server, "out" = server→client) and the snapshot revision observed
// at capture time. Replay feeds "in" entries in logical sequence to a fresh
// server and compares the resulting "out" stream against the recording —
// wall-clock timing never participates.
package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// FormatVersion guards the on-disk shape of replay files.
const FormatVersion = 2

const legacyFormatVersion = 1

// Meta is the P9 header record. Version 1 recordings remain readable, but they
// predate response bindings and cannot establish complete semantic replay.
type Meta struct {
	FormatVersion    int               `json:"formatVersion"`
	ConfigHash       string            `json:"configHash"`
	Backends         map[string]string `json:"backends,omitempty"`  // langID → engine version
	Toolchain        map[string]string `json:"toolchain,omitempty"` // tool → version
	WorkspaceDir     string            `json:"workspaceDir,omitempty"`
	SemanticIdentity *SemanticIdentity `json:"semanticIdentity,omitempty"`
}

// SemanticIdentity records the environment used by a semantic response. A
// generation identity pins an immutable index; a tools-only identity pins a
// live backend path that used no index facts. IndexContentDigest is "sha256:"
// followed by 64 hex digits. Tools records executable identities; BuildContexts
// maps scope or language IDs to digest-backed build context IDs. Empty or
// incomplete values are treated as unverified evidence.
type SemanticIdentity struct {
	Generation         uint64            `json:"generation,omitempty"`
	IndexContentDigest string            `json:"indexContentDigest,omitempty"`
	BuildContexts      map[string]string `json:"buildContexts,omitempty"`
	Tools              []ToolIdentity    `json:"tools,omitempty"`
}

// ToolIdentity records an external tool's concrete identity. SHA256 is the
// executable content hash as 64 hexadecimal digits without a prefix.
type ToolIdentity struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// ReproductionStatus describes the semantic evidence available for a loaded
// session. IdentityPinned means the recording has complete identity metadata;
// IdentityVerified means the metadata matched an available generation. Neither
// status proves that a request response used that generation; Complete also
// requires per-response provenance and a matching replayed binding.
type ReproductionStatus string

const (
	ReproductionPartialUnverified ReproductionStatus = "partial/unverified"
	ReproductionIdentityPinned    ReproductionStatus = "identity-pinned"
	ReproductionIdentityVerified  ReproductionStatus = "identity-verified"
	ReproductionComplete          ReproductionStatus = "complete"
)

var (
	// ErrSemanticIdentityUnverified means a session does not carry enough
	// semantic identity evidence to support full semantic reproduction.
	ErrSemanticIdentityUnverified = errors.New("replay: semantic identity is incomplete or unverified")
	// ErrSemanticGenerationUnavailable means the recorded generation is not
	// available in the replay environment.
	ErrSemanticGenerationUnavailable = errors.New("replay: required semantic generation is unavailable")
	// ErrSemanticIdentityMismatch means the available semantic identity differs
	// from the one captured in the session.
	ErrSemanticIdentityMismatch = errors.New("replay: semantic identity differs from recording")
	// ErrSemanticResponseBindingUnverified means a response lacks enough
	// request-to-generation provenance to support complete semantic replay.
	ErrSemanticResponseBindingUnverified = errors.New("replay: response-to-semantic-generation binding is unavailable")
	// ErrSemanticResponseBindingMismatch means the response binding does not
	// match the response/request IDs or the replayed provenance.
	ErrSemanticResponseBindingMismatch = errors.New("replay: response-to-semantic-generation binding differs from recording")
)

// SemanticBindingKind says whether a response used a semantic index.
type SemanticBindingKind string

const (
	SemanticBindingNone       SemanticBindingKind = "none"
	SemanticBindingGeneration SemanticBindingKind = "generation"
)

// SemanticResponseBinding links one JSON-RPC request/response pair to the
// semantic generation used to produce it. Kind=none is an explicit statement
// that the response did not use semantic-index facts. Generation bindings
// must carry the immutable index-content digest as well as the generation.
type SemanticResponseBinding struct {
	RequestID          jsonrpc.RequestID   `json:"requestId"`
	Kind               SemanticBindingKind `json:"kind"`
	Generation         uint64              `json:"generation,omitempty"`
	IndexContentDigest string              `json:"indexContentDigest,omitempty"`
	ToolIdentityDigest string              `json:"toolIdentityDigest,omitempty"`
}

func newSemanticResponseBinding(id jsonrpc.RequestID, semantic bool, generation uint64, digest string, identity *SemanticIdentity) (SemanticResponseBinding, error) {
	binding := SemanticResponseBinding{RequestID: id, Kind: SemanticBindingNone}
	if !semantic {
		if generation != 0 || digest != "" {
			return SemanticResponseBinding{}, fmt.Errorf("%w: non-semantic binding cannot carry generation data", ErrSemanticResponseBindingMismatch)
		}
		if !completeToolIdentity(identity) {
			return SemanticResponseBinding{}, fmt.Errorf("%w: live semantic response requires a registered backend/tool identity", ErrSemanticIdentityUnverified)
		}
		binding.ToolIdentityDigest = toolIdentityDigest(identity.Tools)
		return binding, nil
	}
	binding.Kind = SemanticBindingGeneration
	binding.Generation = generation
	binding.IndexContentDigest = digest
	if !completeSemanticIdentity(identity) {
		return SemanticResponseBinding{}, fmt.Errorf("%w: indexed response requires a registered complete generation identity", ErrSemanticIdentityUnverified)
	}
	if identity.Generation != generation || identity.IndexContentDigest != digest {
		return SemanticResponseBinding{}, ErrSemanticIdentityMismatch
	}
	return binding, nil
}

// Entry is one recorded protocol message.
type Entry struct {
	Seq             int                      `json:"seq"`
	Dir             string                   `json:"dir"` // in | out | identity
	SnapRev         uint64                   `json:"snapRev,omitempty"`
	SemanticBinding *SemanticResponseBinding `json:"semanticBinding,omitempty"`
	Payload         json.RawMessage          `json:"payload"`
}

// Session is a loaded replay file.
type Session struct {
	Meta    Meta
	Entries []Entry // excludes the meta entry
}

// SemanticReproductionStatus reports what the recording alone can establish.
// A legacy or incomplete header is explicitly partial/unverified; even a
// complete identity tuple is only pinned until checked against an available
// semantic generation.
func (s *Session) SemanticReproductionStatus() ReproductionStatus {
	if s == nil || (!validRegisteredSemanticIdentity(s.Meta.SemanticIdentity) && !s.hasIdentityEvent()) {
		return ReproductionPartialUnverified
	}
	return ReproductionIdentityPinned
}

func (s *Session) hasIdentityEvent() bool {
	if s == nil {
		return false
	}
	for _, entry := range s.Entries {
		if entry.Dir != "identity" {
			continue
		}
		var identity SemanticIdentity
		if json.Unmarshal(entry.Payload, &identity) == nil && validRegisteredSemanticIdentity(&identity) {
			return true
		}
	}
	return false
}

func (s *Session) hasSemanticResponses() bool {
	if s == nil {
		return false
	}
	for _, entry := range s.Entries {
		if entry.Dir != "in" {
			continue
		}
		var msg jsonrpc.Message
		if json.Unmarshal(entry.Payload, &msg) == nil && msg.IsRequest() && responseNeedsSemanticBinding(msg.Method) {
			return true
		}
	}
	return false
}

func (s *Session) usesSemanticGeneration() bool {
	if s == nil {
		return false
	}
	for _, entry := range s.Entries {
		if entry.SemanticBinding != nil && entry.SemanticBinding.Kind == SemanticBindingGeneration {
			return true
		}
	}
	return false
}

// VerifySemanticReproduction checks that the exact semantic generation and
// its content, build-context, and tool identities recorded in the session are
// available. It returns IdentityVerified only after every identity component
// matches. This does not prove that a response used that generation;
// VerifyResponseBindings checks that separately.
func (s *Session) VerifySemanticReproduction(available *SemanticIdentity) (ReproductionStatus, error) {
	if s == nil || !completeSemanticIdentity(s.Meta.SemanticIdentity) {
		return ReproductionPartialUnverified, fmt.Errorf("%w: recording must include a generation, index content digest, build contexts, and tool identities", ErrSemanticIdentityUnverified)
	}
	recorded := s.Meta.SemanticIdentity
	if available == nil || available.Generation == 0 || available.Generation != recorded.Generation {
		got := uint64(0)
		if available != nil {
			got = available.Generation
		}
		return ReproductionPartialUnverified, fmt.Errorf("%w: recording requires generation %d, available generation is %d", ErrSemanticGenerationUnavailable, recorded.Generation, got)
	}
	if !completeSemanticIdentity(available) {
		return ReproductionPartialUnverified, fmt.Errorf("%w: available generation %d lacks complete identity metadata", ErrSemanticIdentityUnverified, available.Generation)
	}
	if !sameSemanticIdentity(recorded, available) {
		return ReproductionPartialUnverified, ErrSemanticIdentityMismatch
	}
	return ReproductionIdentityVerified, nil
}

// VerifyResponseBindings verifies every recorded response has a provenance
// record when its method can use semantic-index facts. The binding must match
// the outstanding request and resolve to a recorded identity event plus an
// independently available generation or tool identity. Legacy sessions
// without required records stay readable but cannot pass this check.
func (s *Session) VerifyResponseBindings(available *SemanticIdentity) error {
	if s == nil {
		return ErrSemanticResponseBindingUnverified
	}
	if err := s.validateReplayFormat(); err != nil {
		return err
	}
	type pendingRequest struct {
		id     jsonrpc.RequestID
		method string
	}
	requests := make(map[string]pendingRequest)
	knownIdentities := make(map[string]SemanticIdentity)
	addIdentity := func(identity *SemanticIdentity) error {
		if !validRegisteredSemanticIdentity(identity) {
			return fmt.Errorf("%w: identity event is incomplete", ErrSemanticIdentityUnverified)
		}
		key := semanticIdentityKey(identity)
		if previous, exists := knownIdentities[key]; exists {
			if identity.Generation > 0 && !sameSemanticIdentity(&previous, identity) {
				return fmt.Errorf("%w: generation %d has conflicting identity events", ErrSemanticIdentityMismatch, identity.Generation)
			}
			return nil
		}
		knownIdentities[key] = *identity
		return nil
	}
	if validRegisteredSemanticIdentity(s.Meta.SemanticIdentity) {
		if err := addIdentity(s.Meta.SemanticIdentity); err != nil {
			return err
		}
	}
	for _, entry := range s.Entries {
		switch entry.Dir {
		case "identity":
			var identity SemanticIdentity
			if err := json.Unmarshal(entry.Payload, &identity); err != nil {
				return fmt.Errorf("replay: identity entry %d unparsable: %w", entry.Seq, err)
			}
			if err := addIdentity(&identity); err != nil {
				return fmt.Errorf("identity entry %d: %w", entry.Seq, err)
			}
		case "in":
			var msg jsonrpc.Message
			if err := json.Unmarshal(entry.Payload, &msg); err != nil {
				return fmt.Errorf("replay: recorded in entry %d unparsable: %w", entry.Seq, err)
			}
			if msg.IsRequest() {
				key := requestIDKey(*msg.ID)
				if _, exists := requests[key]; exists {
					return fmt.Errorf("%w: request ID %s is reused before a response", ErrSemanticResponseBindingMismatch, key)
				}
				requests[key] = pendingRequest{id: *msg.ID, method: msg.Method}
			}
		case "out":
			var msg jsonrpc.Message
			if err := json.Unmarshal(entry.Payload, &msg); err != nil {
				return fmt.Errorf("replay: recorded out entry %d unparsable: %w", entry.Seq, err)
			}
			if !msg.IsResponse() {
				if entry.SemanticBinding != nil {
					return fmt.Errorf("%w: entry %d binding is attached to a non-response", ErrSemanticResponseBindingMismatch, entry.Seq)
				}
				continue
			}
			key := requestIDKey(*msg.ID)
			request, exists := requests[key]
			if !exists || !request.id.Equals(*msg.ID) {
				return fmt.Errorf("%w: response entry %d has no matching outstanding request", ErrSemanticResponseBindingMismatch, entry.Seq)
			}
			delete(requests, key)
			binding := entry.SemanticBinding
			if responseNeedsSemanticBinding(request.method) && binding == nil {
				return fmt.Errorf("%w: semantic response entry %d (%s) has no binding", ErrSemanticResponseBindingUnverified, entry.Seq, request.method)
			}
			if binding != nil {
				if !binding.RequestID.Equals(*msg.ID) {
					return fmt.Errorf("%w: response entry %d ID does not match its binding", ErrSemanticResponseBindingMismatch, entry.Seq)
				}
				if err := verifySemanticResponseBinding(binding, available); err != nil {
					return fmt.Errorf("response entry %d: %w", entry.Seq, err)
				}
				identity, err := s.identityForBinding(binding, knownIdentities)
				if err != nil {
					return fmt.Errorf("response entry %d: %w", entry.Seq, err)
				}
				if binding.Kind == SemanticBindingGeneration && !sameSemanticIdentity(identity, available) {
					return fmt.Errorf("response entry %d: %w", entry.Seq, ErrSemanticIdentityMismatch)
				}
				if binding.Kind == SemanticBindingNone && !sameToolIdentity(identity.Tools, available.Tools) {
					return fmt.Errorf("response entry %d: %w", entry.Seq, ErrSemanticIdentityMismatch)
				}
			}
		}
	}
	for _, request := range requests {
		if responseNeedsSemanticBinding(request.method) {
			return fmt.Errorf("%w: semantic request %s has no recorded response", ErrSemanticResponseBindingUnverified, request.method)
		}
	}
	return nil
}

func (s *Session) validateReplayFormat() error {
	if s == nil || (s.Meta.FormatVersion != FormatVersion && s.Meta.FormatVersion != legacyFormatVersion) {
		version := 0
		if s != nil {
			version = s.Meta.FormatVersion
		}
		return fmt.Errorf("replay: format v%d unsupported (want v%d)", version, FormatVersion)
	}
	if s.Meta.FormatVersion == legacyFormatVersion {
		for _, entry := range s.Entries {
			if entry.Dir == "identity" || entry.SemanticBinding != nil {
				return fmt.Errorf("%w: format v%d cannot carry response identity evidence", ErrSemanticResponseBindingUnverified, legacyFormatVersion)
			}
		}
	}
	return nil
}

// responseNeedsSemanticBinding classifies language queries that can use a
// semantic index. Old sessions can omit annotations on protocol/control
// responses, while index-capable methods require an explicit none or
// generation decision.
func responseNeedsSemanticBinding(method string) bool {
	switch method {
	case "textDocument/hover",
		"textDocument/completion",
		"textDocument/definition",
		"textDocument/declaration",
		"textDocument/documentSymbol",
		"textDocument/references",
		"textDocument/rename",
		"textDocument/semanticTokens/full",
		"textDocument/prepareRename",
		"textDocument/diagnostic",
		"textDocument/codeAction",
		"textDocument/signatureHelp",
		"textDocument/formatting",
		"textDocument/inlayHint",
		"workspace/symbol":
		return true
	default:
		return false
	}
}

func verifySemanticResponseBinding(binding *SemanticResponseBinding, available *SemanticIdentity) error {
	if binding == nil {
		return ErrSemanticResponseBindingUnverified
	}
	switch binding.Kind {
	case SemanticBindingNone:
		if binding.Generation != 0 || binding.IndexContentDigest != "" {
			return fmt.Errorf("%w: non-semantic binding carries generation data", ErrSemanticResponseBindingMismatch)
		}
		if !validPrefixedSHA256(binding.ToolIdentityDigest) || !completeToolIdentity(available) {
			return fmt.Errorf("%w: live response binding requires a tool identity digest", ErrSemanticIdentityUnverified)
		}
		if binding.ToolIdentityDigest != toolIdentityDigest(available.Tools) {
			return ErrSemanticIdentityMismatch
		}
		return nil
	case SemanticBindingGeneration:
		if binding.Generation == 0 || !validPrefixedSHA256(binding.IndexContentDigest) {
			return fmt.Errorf("%w: semantic binding must include a generation and sha256 digest", ErrSemanticResponseBindingUnverified)
		}
		if available == nil || available.Generation == 0 {
			return fmt.Errorf("%w: no semantic generation is available", ErrSemanticGenerationUnavailable)
		}
		if binding.Generation != available.Generation {
			return fmt.Errorf("%w: response requires generation %d, available generation is %d", ErrSemanticGenerationUnavailable, binding.Generation, available.Generation)
		}
		if binding.IndexContentDigest != available.IndexContentDigest {
			return ErrSemanticIdentityMismatch
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown binding kind %q", ErrSemanticResponseBindingUnverified, binding.Kind)
	}
}

func requestIDKey(id jsonrpc.RequestID) string {
	b, _ := json.Marshal(id)
	return string(b)
}

func completeSemanticIdentity(identity *SemanticIdentity) bool {
	if identity == nil || identity.Generation == 0 || !validPrefixedSHA256(identity.IndexContentDigest) ||
		len(identity.BuildContexts) == 0 || !completeToolIdentity(identity) {
		return false
	}
	for key, value := range identity.BuildContexts {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func completeToolIdentity(identity *SemanticIdentity) bool {
	if identity == nil || len(identity.Tools) == 0 {
		return false
	}
	for _, tool := range identity.Tools {
		if strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Path) == "" ||
			strings.TrimSpace(tool.Version) == "" || !validSHA256Hex(tool.SHA256) {
			return false
		}
	}
	return true
}

func sameToolIdentity(a, b []ToolIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	toolsA := append([]ToolIdentity(nil), a...)
	toolsB := append([]ToolIdentity(nil), b...)
	sortTools(toolsA)
	sortTools(toolsB)
	for i := range toolsA {
		if toolsA[i] != toolsB[i] {
			return false
		}
	}
	return true
}

func toolIdentityDigest(tools []ToolIdentity) string {
	ordered := append([]ToolIdentity(nil), tools...)
	sortTools(ordered)
	data, _ := json.Marshal(ordered)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func semanticIdentityKey(identity *SemanticIdentity) string {
	if identity == nil {
		return ""
	}
	if identity.Generation > 0 {
		return fmt.Sprintf("generation:%d", identity.Generation)
	}
	if completeToolIdentity(identity) {
		return "tools:" + toolIdentityDigest(identity.Tools)
	}
	return ""
}

func validRegisteredSemanticIdentity(identity *SemanticIdentity) bool {
	if identity == nil {
		return false
	}
	if identity.Generation == 0 {
		return completeToolIdentity(identity) && identity.IndexContentDigest == "" && len(identity.BuildContexts) == 0
	}
	return completeSemanticIdentity(identity)
}

func cloneSemanticIdentity(identity SemanticIdentity) SemanticIdentity {
	copy := identity
	if identity.BuildContexts != nil {
		copy.BuildContexts = make(map[string]string, len(identity.BuildContexts))
		for key, value := range identity.BuildContexts {
			copy.BuildContexts[key] = value
		}
	}
	copy.Tools = append([]ToolIdentity(nil), identity.Tools...)
	return copy
}

func registerSemanticIdentity(registry map[string]SemanticIdentity, identity SemanticIdentity) (string, error) {
	if !validRegisteredSemanticIdentity(&identity) {
		return "", fmt.Errorf("%w: identity must include a complete generation or backend/tool identity", ErrSemanticIdentityUnverified)
	}
	key := semanticIdentityKey(&identity)
	if prior, exists := registry[key]; exists {
		if identity.Generation > 0 && !sameSemanticIdentity(&prior, &identity) {
			return "", fmt.Errorf("%w: generation %d has conflicting identities", ErrSemanticIdentityMismatch, identity.Generation)
		}
		return key, nil
	}
	if identity.Generation > 0 {
		for _, prior := range registry {
			if prior.Generation == identity.Generation && !sameSemanticIdentity(&prior, &identity) {
				return "", fmt.Errorf("%w: generation %d has conflicting identities", ErrSemanticIdentityMismatch, identity.Generation)
			}
		}
	}
	registry[key] = cloneSemanticIdentity(identity)
	return key, nil
}

func (s *Session) identityForBinding(binding *SemanticResponseBinding, prior map[string]SemanticIdentity) (*SemanticIdentity, error) {
	if binding == nil {
		return nil, ErrSemanticResponseBindingUnverified
	}
	switch binding.Kind {
	case SemanticBindingGeneration:
		for _, identity := range prior {
			if identity.Generation == binding.Generation && identity.IndexContentDigest == binding.IndexContentDigest {
				return &identity, nil
			}
		}
		return nil, fmt.Errorf("%w: no identity event for generation %d", ErrSemanticIdentityUnverified, binding.Generation)
	case SemanticBindingNone:
		for _, identity := range prior {
			if completeToolIdentity(&identity) && toolIdentityDigest(identity.Tools) == binding.ToolIdentityDigest {
				return &identity, nil
			}
		}
		return nil, fmt.Errorf("%w: no identity event for tool digest %q", ErrSemanticIdentityUnverified, binding.ToolIdentityDigest)
	default:
		return nil, fmt.Errorf("%w: unknown binding kind %q", ErrSemanticResponseBindingUnverified, binding.Kind)
	}
}

func sameSemanticIdentity(a, b *SemanticIdentity) bool {
	if a.Generation != b.Generation || a.IndexContentDigest != b.IndexContentDigest ||
		len(a.BuildContexts) != len(b.BuildContexts) || len(a.Tools) != len(b.Tools) {
		return false
	}
	for key, value := range a.BuildContexts {
		if b.BuildContexts[key] != value {
			return false
		}
	}
	toolsA := append([]ToolIdentity(nil), a.Tools...)
	toolsB := append([]ToolIdentity(nil), b.Tools...)
	sortTools(toolsA)
	sortTools(toolsB)
	for i := range toolsA {
		if toolsA[i] != toolsB[i] {
			return false
		}
	}
	return true
}

func sortTools(tools []ToolIdentity) {
	sort.Slice(tools, func(i, j int) bool {
		a, b := tools[i], tools[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.SHA256 < b.SHA256
	})
}

func validPrefixedSHA256(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return validSHA256Hex(strings.TrimPrefix(value, prefix))
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ConfigHash derives the P9 config digest from any deterministic value.
func ConfigHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Save writes the session as JSONL (meta first, then entries in order).
func Save(path string, s *Session) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("replay: create %q: %w", path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	metaEntry := Entry{Seq: 0, Dir: "meta", Payload: mustJSON(s.Meta)}
	if err := enc.Encode(metaEntry); err != nil {
		return err
	}
	for _, e := range s.Entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return w.Flush()
}

// LoadSession parses a JSONL replay file.
func LoadSession(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open %q: %w", path, err)
	}
	defer f.Close()

	s := &Session{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // large didChange payloads
	line := 0
	metaSeen := false
	for sc.Scan() {
		line++
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("replay: %s:%d: %w", path, line, err)
		}
		switch e.Dir {
		case "meta":
			if line != 1 || metaSeen {
				return nil, fmt.Errorf("replay: %s:%d meta header must appear exactly once on the first line", path, line)
			}
			if err := json.Unmarshal(e.Payload, &s.Meta); err != nil {
				return nil, fmt.Errorf("replay: %s:%d meta: %w", path, line, err)
			}
			if s.Meta.FormatVersion != FormatVersion && s.Meta.FormatVersion != legacyFormatVersion {
				return nil, fmt.Errorf("replay: format v%d unsupported (want v%d)",
					s.Meta.FormatVersion, FormatVersion)
			}
			metaSeen = true
		case "in", "out", "identity":
			if !metaSeen {
				return nil, fmt.Errorf("replay: %s:%d missing meta header", path, line)
			}
			s.Entries = append(s.Entries, e)
		default:
			return nil, fmt.Errorf("replay: %s:%d unknown dir %q", path, line, e.Dir)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !metaSeen {
		return nil, fmt.Errorf("replay: %s: missing meta header", path)
	}
	return s, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
