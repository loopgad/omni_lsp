// Package semantic stores and reads versioned semantic facts in persistent
// generation segments. The persistent package remains format agnostic.
//
// Invariants: a reader validates the whole generation before yielding facts;
// source identity, build context, bounds and required coverage are checked.
package semantic

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
)

const (
	// SchemaVersion versions the semantic payload envelope and metadata record.
	SchemaVersion uint32 = 1
	// MaxRecordBytes limits the JSON encoding of any one fact or metadata item.
	MaxRecordBytes = 1 << 20
	// MaxBatchBytes limits each encoded symbols, occurrences, or edges payload.
	MaxBatchBytes = 8 << 20
	// MaxSegmentBytes limits the payload passed to persistent.BuildSession.
	MaxSegmentBytes = 64 << 20

	semanticFormat = "omnilsp-semantic-segment"
)

var (
	ErrNotSemantic       = errors.New("semantic: generation does not contain semantic facts")
	ErrMalformedPayload  = errors.New("semantic: malformed or corrupt payload")
	ErrUnsupportedSchema = errors.New("semantic: unsupported schema version")
	ErrBounds            = errors.New("semantic: payload exceeds a format bound")
	ErrInvalidScope      = errors.New("semantic: invalid scope ID or scope")
	ErrInvalidSource     = errors.New("semantic: invalid source URI or content hash")
	ErrBuildContext      = errors.New("semantic: fact has an unknown or mismatched build context")
	ErrUnknownSymbol     = errors.New("semantic: occurrence or edge references an unknown symbol")
	ErrDuplicateSymbol   = errors.New("semantic: symbol ID is declared in multiple scopes")
	ErrSinkFinalized     = errors.New("semantic: sink is finalized or failed")
)

// NotSemanticError distinguishes an inventory/legacy generation from a
// semantic generation. It supports errors.Is(err, ErrNotSemantic).
type NotSemanticError struct {
	SegmentID persistent.SegmentID
	Reason    string
}

func (e *NotSemanticError) Error() string {
	if e == nil {
		return ErrNotSemantic.Error()
	}
	if e.Reason == "" {
		return fmt.Sprintf("%s: segment %s", ErrNotSemantic, e.SegmentID)
	}
	return fmt.Sprintf("%s: segment %s: %s", ErrNotSemantic, e.SegmentID, e.Reason)
}

func (e *NotSemanticError) Unwrap() error { return ErrNotSemantic }

// Metadata binds the semantic records to their immutable workspace snapshot
// and the complete extraction report. SnapshotRev is persisted as zero because
// it is a process-local build guard, not a durable cache identity. DiskDigest
// is repeated explicitly for direct source-tree freshness checks.
type Metadata struct {
	SchemaVersion uint32                          `json:"schemaVersion"`
	Identity      model.Identity                  `json:"identity"`
	DiskDigest    identity.ContentHash            `json:"diskDigest"`
	Scopes        []model.Scope                   `json:"scopes"`
	Provenance    map[string]model.Provenance     `json:"provenance"`
	Coverage      []model.Coverage                `json:"coverage"`
	UsedTools     map[string][]model.ToolIdentity `json:"usedTools"`
}

type BatchKind string

const (
	BatchSymbols     BatchKind = "symbols"
	BatchOccurrences BatchKind = "occurrences"
	BatchEdges       BatchKind = "edges"
)

// Batch is one decoded typed segment. Exactly one fact slice is populated.
type Batch struct {
	Kind        BatchKind
	Symbols     []model.Symbol
	Occurrences []model.Occurrence
	Edges       []model.Edge
}

type wireSegment struct {
	Format        string          `json:"format"`
	SchemaVersion uint32          `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	Records       json.RawMessage `json:"records,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// Sink implements model.Sink and writes each bounded batch directly to the
// supplied build session. Finalize validates the merged report and writes the
// metadata segment; the caller may commit only after Finalize succeeds.
type Sink struct {
	mu           sync.Mutex
	build        persistent.BuildSession
	req          model.Request
	identity     model.Identity
	scopes       map[string]model.Scope
	sourceHashes map[string]map[string]identity.ContentHash
	factScopes   map[string]struct{}
	ledger       *idLedger
	state        sinkState
	result       error
}

type sinkState uint8

const (
	sinkOpen sinkState = iota
	sinkFinalized
	sinkFailed
	sinkClosed
)

// NewSink captures and validates the request scope and immutable source-file
// manifest, then returns a streaming model.Sink. The caller passes one
// aggregate request for all scopes/providers in this generation.
func NewSink(ctx context.Context, build persistent.BuildSession, req model.Request) (*Sink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if build == nil {
		return nil, errors.New("semantic: nil build session")
	}
	if req.View == nil {
		return nil, model.ErrMissingView
	}
	id := req.View.Identity()
	if id.Workspace == "" || id.DiskDigest == "" {
		return nil, fmt.Errorf("%w: missing workspace or disk digest", ErrInvalidSource)
	}
	if !validContentHash(id.DiskDigest) {
		return nil, fmt.Errorf("%w: malformed disk digest", ErrInvalidSource)
	}
	if len(req.Scopes) == 0 {
		return nil, ErrInvalidScope
	}
	cloned := cloneRequest(req)
	s := &Sink{
		build:        build,
		req:          cloned,
		identity:     id,
		scopes:       make(map[string]model.Scope, len(req.Scopes)),
		sourceHashes: make(map[string]map[string]identity.ContentHash, len(req.Scopes)),
		factScopes:   make(map[string]struct{}),
	}
	for _, scope := range cloned.Scopes {
		if !validScope(scope) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidScope, scope.ID)
		}
		if _, exists := s.scopes[scope.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate scope %q", ErrInvalidScope, scope.ID)
		}
		provenance, ok := cloned.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != id ||
			!reflect.DeepEqual(provenance.Scope, scope) || provenance.Extractor == "" || provenance.ExtractorVer == "" ||
			provenance.Toolchain == "" || provenance.Backend.Name == "" ||
			provenance.Backend.Language != scope.Language ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return nil, fmt.Errorf("%w: scope %q provenance or BuildContext", model.ErrInvalidProvenance, scope.ID)
		}
		s.scopes[scope.ID] = scope
		s.sourceHashes[scope.ID] = make(map[string]identity.ContentHash)
	}
	for scopeID := range cloned.Provenance {
		if _, ok := s.scopes[scopeID]; !ok {
			return nil, fmt.Errorf("%w: provenance for unknown scope %q", model.ErrInvalidProvenance, scopeID)
		}
	}

	for _, scope := range cloned.Scopes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := cloned.View.Walk(ctx, scope.RootURI, func(file model.File) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !uriWithinRoot(scope.RootURI, file.URI) || !validContentHash(file.SHA256) {
				return fmt.Errorf("%w: invalid workspace file %q", ErrInvalidSource, file.URI)
			}
			files := s.sourceHashes[scope.ID]
			if prior, ok := files[file.URI]; ok && prior != file.SHA256 {
				return fmt.Errorf("%w: conflicting hashes for %q", ErrInvalidSource, file.URI)
			}
			files[file.URI] = file.SHA256
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("semantic: capture source manifest for %q: %w", scope.ID, err)
		}
	}
	ledger, err := newIDLedger()
	if err != nil {
		return nil, err
	}
	s.ledger = ledger
	return s, nil
}

// WriteSymbols validates and writes symbols in bounded typed batches.
func (s *Sink) WriteSymbols(ctx context.Context, values []model.Symbol) error {
	return s.writeRecords(ctx, BatchSymbols, len(values), func(i int) (any, error) {
		v := values[i]
		if err := s.validateScopeID(v.ScopeID); err != nil {
			return nil, err
		}
		if v.ID == "" || !validIdentifier(string(v.ID)) || !validString(v.Name) || !validString(v.Kind) || !validString(v.Signature) {
			return nil, fmt.Errorf("%w: invalid symbol record", ErrMalformedPayload)
		}
		return v, nil
	})
}

// WriteOccurrences validates source identity and writes occurrences in bounded
// typed batches. Symbol references may precede their definitions; Finalize
// verifies all references before metadata can be written.
func (s *Sink) WriteOccurrences(ctx context.Context, values []model.Occurrence) error {
	return s.writeRecords(ctx, BatchOccurrences, len(values), func(i int) (any, error) {
		v := values[i]
		if err := s.validateScopeID(v.ScopeID); err != nil {
			return nil, err
		}
		if v.SymbolID == "" || !validIdentifier(string(v.SymbolID)) || !validString(v.Role) || !validPosition(v.Range) {
			return nil, fmt.Errorf("%w: invalid occurrence record", ErrMalformedPayload)
		}
		if err := s.validateBuildContext(v.ScopeID, v.BuildContext); err != nil {
			return nil, err
		}
		if err := s.validateSource(v.ScopeID, v.URI, v.SourceHash); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// WriteEdges validates source identity and writes edges in bounded typed
// batches. Both endpoints must resolve to symbols in the same scope by
// Finalize.
func (s *Sink) WriteEdges(ctx context.Context, values []model.Edge) error {
	return s.writeRecords(ctx, BatchEdges, len(values), func(i int) (any, error) {
		v := values[i]
		if err := s.validateScopeID(v.ScopeID); err != nil {
			return nil, err
		}
		if v.From == "" || v.To == "" || !validIdentifier(string(v.From)) || !validIdentifier(string(v.To)) ||
			!validString(string(v.Kind)) || !validPosition(v.Range) || !validString(v.SourceURI) {
			return nil, fmt.Errorf("%w: invalid edge record", ErrMalformedPayload)
		}
		if err := s.validateBuildContext(v.ScopeID, v.BuildContext); err != nil {
			return nil, err
		}
		if err := s.validateSource(v.ScopeID, v.SourceURI, v.SourceHash); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// Finalize validates extraction coverage/provenance and all cross-record
// symbol references, then writes the required metadata segment.
func (s *Sink) Finalize(ctx context.Context, report model.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sinkOpen {
		if s.result != nil {
			return s.result
		}
		return ErrSinkFinalized
	}
	if err := ctx.Err(); err != nil {
		return s.fail(err)
	}
	if s.req.View.Identity() != s.identity || report.Identity != s.identity {
		return s.fail(model.ErrSnapshotMismatch)
	}
	if err := model.ValidateReport(s.req, report); err != nil {
		return s.fail(err)
	}
	for scopeID := range s.factScopes {
		if !model.HasPinnedToolUsage(s.req.Provenance[scopeID].Tools, report.UsedTools[scopeID]) {
			return s.fail(model.ErrInvalidProvenance)
		}
	}
	if err := s.ledger.Validate(ctx, report.Coverage); err != nil {
		return s.fail(err)
	}
	meta := Metadata{
		SchemaVersion: SchemaVersion,
		Identity:      durableIdentity(s.identity),
		DiskDigest:    s.identity.DiskDigest,
		Scopes:        cloneScopes(s.req.Scopes),
		Provenance:    cloneProvenance(s.req.Provenance),
		Coverage:      append([]model.Coverage(nil), report.Coverage...),
		UsedTools:     cloneUsedTools(report.UsedTools),
	}
	for scopeID, provenance := range meta.Provenance {
		provenance.Identity = durableIdentity(provenance.Identity)
		meta.Provenance[scopeID] = provenance
	}
	if err := validateMetadataRecords(meta); err != nil {
		return s.fail(err)
	}
	data, err := encodeMetadata(meta)
	if err != nil {
		return s.fail(err)
	}
	if len(data) > MaxSegmentBytes {
		return s.fail(fmt.Errorf("%w: metadata segment is %d bytes (limit %d)", ErrBounds, len(data), MaxSegmentBytes))
	}
	if err := ctx.Err(); err != nil {
		return s.fail(err)
	}
	if _, err := s.build.WriteSegment(data); err != nil {
		return s.fail(err)
	}
	s.state = sinkFinalized
	s.result = nil
	_ = s.ledger.Close()
	return nil
}

// Close releases the temporary cross-reference ledger. It is safe after
// Finalize and should be called when an extraction is abandoned.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == sinkClosed {
		return nil
	}
	s.state = sinkClosed
	if s.ledger != nil {
		return s.ledger.Close()
	}
	return nil
}

func (s *Sink) writeRecords(ctx context.Context, kind BatchKind, count int, record func(int) (any, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sinkOpen {
		if s.result != nil {
			return s.result
		}
		return ErrSinkFinalized
	}
	if err := ctx.Err(); err != nil {
		return s.fail(err)
	}
	// Validate the complete caller batch before publishing any of its pieces.
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return s.fail(err)
		}
		v, err := record(i)
		if err != nil {
			return s.fail(err)
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return s.fail(err)
		}
		if len(encoded) > MaxRecordBytes {
			return s.fail(fmt.Errorf("%w: %s record is %d bytes (limit %d)", ErrBounds, kind, len(encoded), MaxRecordBytes))
		}
	}
	for start := 0; start < count; {
		if err := ctx.Err(); err != nil {
			return s.fail(err)
		}
		rawRecords := make([]json.RawMessage, 0, 256)
		used := 0
		end := start
		for end < count {
			v, err := record(end)
			if err != nil {
				return s.fail(err)
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return s.fail(err)
			}
			estimated := used + len(raw)
			if len(rawRecords) != 0 {
				estimated++ // comma
			}
			// Envelope field names, schema and braces are below 128 bytes.
			if len(rawRecords) != 0 && estimated+128 > MaxBatchBytes {
				break
			}
			rawRecords = append(rawRecords, raw)
			used = estimated
			end++
		}
		data, err := encodeBatch(kind, rawRecords)
		if err != nil {
			return s.fail(err)
		}
		if len(data) > MaxBatchBytes || len(data) > MaxSegmentBytes {
			return s.fail(fmt.Errorf("%w: encoded %s batch is %d bytes", ErrBounds, kind, len(data)))
		}
		if err := ctx.Err(); err != nil {
			return s.fail(err)
		}
		if _, err := s.build.WriteSegment(data); err != nil {
			return s.fail(err)
		}
		if err := s.recordIDs(kind, start, end, record); err != nil {
			return s.fail(err)
		}
		start = end
	}
	return nil
}

func (s *Sink) recordIDs(kind BatchKind, start, end int, record func(int) (any, error)) error {
	for i := start; i < end; i++ {
		value, err := record(i)
		if err != nil {
			return err
		}
		switch kind {
		case BatchSymbols:
			v := value.(model.Symbol)
			if err := s.ledger.Add(v.ScopeID, string(v.ID), idDeclared, ""); err != nil {
				return err
			}
			s.factScopes[v.ScopeID] = struct{}{}
		case BatchOccurrences:
			v := value.(model.Occurrence)
			if err := s.ledger.Add(v.ScopeID, string(v.SymbolID), idReferenced, occurrenceFact(v.Role)); err != nil {
				return err
			}
			s.factScopes[v.ScopeID] = struct{}{}
		case BatchEdges:
			v := value.(model.Edge)
			fact, ok := edgeFact(v.Kind)
			if !ok {
				return fmt.Errorf("%w: unsupported edge kind %q", ErrMalformedPayload, v.Kind)
			}
			if err := s.ledger.Add(v.ScopeID, string(v.From), idReferenced, fact); err != nil {
				return err
			}
			if err := s.ledger.Add(v.ScopeID, string(v.To), idReferenced, fact); err != nil {
				return err
			}
			s.factScopes[v.ScopeID] = struct{}{}
		}
	}
	return nil
}

func (s *Sink) validateScopeID(scopeID string) error {
	if !validIdentifier(scopeID) || scopeID == "" {
		return ErrInvalidScope
	}
	if _, ok := s.scopes[scopeID]; !ok {
		return fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, scopeID)
	}
	return nil
}

func (s *Sink) validateBuildContext(scopeID string, actual identity.BuildContextID) error {
	scope, ok := s.scopes[scopeID]
	if !ok || actual == "" || actual != scope.BuildContext {
		return fmt.Errorf("%w: scope %q", ErrBuildContext, scopeID)
	}
	return nil
}

func (s *Sink) validateSource(scopeID, uri string, hash identity.ContentHash) error {
	if !validString(uri) || hash == "" || len(hash) > MaxRecordBytes {
		return fmt.Errorf("%w: empty, oversized, or invalid source identity", ErrInvalidSource)
	}
	files := s.sourceHashes[scopeID]
	want, ok := files[uri]
	if !ok || want != hash || !uriWithinRoot(s.scopes[scopeID].RootURI, uri) {
		return fmt.Errorf("%w: URI/hash do not match captured file %q", ErrInvalidSource, uri)
	}
	return nil
}

func (s *Sink) fail(err error) error {
	if s.state == sinkOpen {
		s.state = sinkFailed
		s.result = err
		if s.ledger != nil {
			_ = s.ledger.Close()
		}
	}
	return err
}

func encodeBatch(kind BatchKind, records []json.RawMessage) ([]byte, error) {
	encodedRecords, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	wire := wireSegment{Format: semanticFormat, SchemaVersion: SchemaVersion, Kind: string(kind), Records: encodedRecords}
	return json.Marshal(wire)
}

func encodeMetadata(meta Metadata) ([]byte, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	wire := wireSegment{Format: semanticFormat, SchemaVersion: SchemaVersion, Kind: "metadata", Metadata: encoded}
	return json.Marshal(wire)
}

func decodeWire(payload []byte, segmentID persistent.SegmentID) (wireSegment, error) {
	if len(payload) > MaxSegmentBytes {
		return wireSegment{}, fmt.Errorf("%w: segment %s is %d bytes (limit %d)", ErrBounds, segmentID, len(payload), MaxSegmentBytes)
	}
	if isLegacySealedInventory(payload) {
		return wireSegment{}, &NotSemanticError{SegmentID: segmentID, Reason: "legacy sealed file inventory"}
	}
	if !utf8.Valid(payload) {
		return wireSegment{}, fmt.Errorf("%w: segment %s is not UTF-8", ErrMalformedPayload, segmentID)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return wireSegment{}, &NotSemanticError{SegmentID: segmentID, Reason: "empty legacy payload"}
	}
	if trimmed[0] == '[' && isLegacyInventory(trimmed) {
		return wireSegment{}, &NotSemanticError{SegmentID: segmentID, Reason: "legacy file inventory"}
	}
	var wire wireSegment
	if err := decodeStrict(trimmed, &wire); err != nil {
		return wireSegment{}, fmt.Errorf("%w: segment %s: %v", ErrMalformedPayload, segmentID, err)
	}
	if wire.Format != semanticFormat {
		return wireSegment{}, &NotSemanticError{SegmentID: segmentID, Reason: "unrecognized segment format"}
	}
	if wire.SchemaVersion != SchemaVersion {
		return wireSegment{}, fmt.Errorf("%w: segment %s has schema %d", ErrUnsupportedSchema, segmentID, wire.SchemaVersion)
	}
	return wire, nil
}

func isLegacySealedInventory(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if n == 0 || n > 4096 || uint64(len(payload)) < 4+uint64(n) {
		return false
	}
	var tuple persistent.FreshnessTuple
	if err := decodeStrict(payload[4:4+n], &tuple); err != nil || tuple.SourceHash == "" && tuple.BackendVer == "" {
		return false
	}
	return isLegacyInventory(bytes.TrimSpace(payload[4+n:]))
}

func isLegacyInventory(payload []byte) bool {
	var records []map[string]json.RawMessage
	if json.Unmarshal(payload, &records) != nil {
		return false
	}
	if len(records) == 0 {
		return true
	}
	for _, record := range records {
		if _, upper := record["Path"]; !upper {
			if _, lower := record["path"]; !lower {
				return false
			}
		}
		if _, upper := record["Size"]; !upper {
			if _, lower := record["size"]; !lower {
				return false
			}
		}
		if _, upper := record["SHA256"]; !upper {
			if _, lower := record["sha256"]; !lower {
				return false
			}
		}
	}
	return true
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validScope(scope model.Scope) bool {
	return scope.ID != "" && scope.Language != "" && validIdentifier(scope.ID) && validIdentifier(scope.Language) &&
		scope.BuildContext != "" && validString(string(scope.BuildContext)) && uriValid(scope.RootURI)
}

func validPosition(pos model.Position) bool {
	return pos.EndLine > pos.StartLine || (pos.EndLine == pos.StartLine && pos.EndChar >= pos.StartChar)
}

func validString(value string) bool { return utf8.ValidString(value) && len(value) <= MaxRecordBytes }

func validIdentifier(value string) bool {
	return validString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validContentHash(value identity.ContentHash) bool {
	text := string(value)
	if len(text) != len("sha256:")+64 || !strings.HasPrefix(text, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(text[len("sha256:"):])
	return err == nil && len(decoded) == 32
}

func occurrenceFact(role string) model.FactKind {
	switch strings.ToLower(role) {
	case "definition":
		return model.FactDefinition
	case "declaration":
		return model.FactDeclaration
	default:
		return model.FactReference
	}
}

func edgeFact(kind model.EdgeKind) (model.FactKind, bool) {
	switch kind {
	case model.EdgeImplementation:
		return model.FactImplementation, true
	case model.EdgeTypeRelation:
		return model.FactTypeRelation, true
	case model.EdgeCall:
		return model.FactCall, true
	case model.EdgeImport:
		return model.FactImport, true
	case model.EdgeInclude:
		return model.FactInclude, true
	case model.EdgeModule:
		return model.FactModule, true
	case model.EdgeGenerated:
		return model.FactGenerated, true
	default:
		return "", false
	}
}

func uriValid(value string) bool {
	if value == "" || !validString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && u.IsAbs() && u.Opaque == "" && u.Scheme != ""
}

func uriWithinRoot(root, candidate string) bool {
	if !uriValid(root) || !uriValid(candidate) {
		return false
	}
	r, err := url.Parse(root)
	if err != nil {
		return false
	}
	c, err := url.Parse(candidate)
	if err != nil || !strings.EqualFold(r.Scheme, c.Scheme) || !strings.EqualFold(r.Host, c.Host) {
		return false
	}
	rootPath := path.Clean(r.Path)
	filePath := path.Clean(c.Path)
	if rootPath == "." {
		rootPath = "/"
	}
	if filePath == rootPath {
		return true
	}
	if rootPath == "/" {
		return strings.HasPrefix(filePath, "/")
	}
	return strings.HasPrefix(filePath, strings.TrimSuffix(rootPath, "/")+"/")
}

func validateMetadataRecords(meta Metadata) error {
	total := int64(256) // envelope, identity, and JSON container overhead
	add := func(size int) error {
		if size < 0 || total > MaxSegmentBytes-int64(size)-2 {
			return fmt.Errorf("%w: metadata segment exceeds %d bytes", ErrBounds, MaxSegmentBytes)
		}
		total += int64(size) + 2 // commas and quoting/field separators
		return nil
	}
	if err := checkAndAddRecord(add, meta.Identity); err != nil {
		return err
	}
	if err := add(len(meta.DiskDigest)); err != nil {
		return err
	}
	for _, scope := range meta.Scopes {
		if err := checkAndAddRecord(add, scope); err != nil {
			return fmt.Errorf("scope %q: %w", scope.ID, err)
		}
		provenance, ok := meta.Provenance[scope.ID]
		if !ok {
			return fmt.Errorf("%w: no provenance for %q", model.ErrInvalidProvenance, scope.ID)
		}
		key, err := json.Marshal(scope.ID)
		if err != nil {
			return err
		}
		if err := add(len(key)); err != nil {
			return err
		}
		if err := checkAndAddRecord(add, provenance); err != nil {
			return fmt.Errorf("provenance for %q: %w", scope.ID, err)
		}
	}
	for _, coverage := range meta.Coverage {
		if err := checkAndAddRecord(add, coverage); err != nil {
			return fmt.Errorf("coverage for %q: %w", coverage.ScopeID, err)
		}
	}
	for scopeID, tools := range meta.UsedTools {
		key, err := json.Marshal(scopeID)
		if err != nil {
			return err
		}
		if err := add(len(key)); err != nil {
			return err
		}
		for _, tool := range tools {
			if err := checkAndAddRecord(add, struct {
				ScopeID string
				Tool    model.ToolIdentity
			}{scopeID, tool}); err != nil {
				return fmt.Errorf("used tool for %q: %w", scopeID, err)
			}
		}
	}
	if total > MaxSegmentBytes {
		return fmt.Errorf("%w: metadata segment exceeds %d bytes", ErrBounds, MaxSegmentBytes)
	}
	return nil
}

func checkAndAddRecord(add func(int) error, value any) error {
	if !allStringsValid(value) {
		return fmt.Errorf("%w: invalid UTF-8 string", ErrMalformedPayload)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > MaxRecordBytes {
		return fmt.Errorf("%w: metadata record is %d bytes (limit %d)", ErrBounds, len(data), MaxRecordBytes)
	}
	return add(len(data))
}

func allStringsValid(value any) bool {
	switch v := value.(type) {
	case string:
		return utf8.ValidString(v)
	case []string:
		for _, item := range v {
			if !utf8.ValidString(item) {
				return false
			}
		}
	case map[string]string:
		for key, item := range v {
			if !utf8.ValidString(key) || !utf8.ValidString(item) {
				return false
			}
		}
	case model.Scope:
		return utf8.ValidString(v.ID) && utf8.ValidString(v.Language) && utf8.ValidString(v.RootURI) &&
			utf8.ValidString(string(v.BuildContext)) && allStringsValid(v.Build)
	case model.Identity:
		return utf8.ValidString(string(v.Workspace)) && utf8.ValidString(string(v.DiskDigest))
	case model.BuildInputs:
		return allStringsValid(v.Environment) && allStringsValid(v.Arguments) && allStringsValid(v.PackagePatterns) &&
			allStringsValid(v.IncludePaths) && allStringsValid(v.Defines) && allStringsValid(v.Features) && allStringsValid(v.Options)
	case model.Provenance:
		if !utf8.ValidString(v.Extractor) || !utf8.ValidString(v.ExtractorVer) || !utf8.ValidString(v.Toolchain) ||
			!utf8.ValidString(v.Backend.Language) || !utf8.ValidString(v.Backend.Name) || !allStringsValid(v.Identity) {
			return false
		}
		for _, tool := range v.Tools {
			if !allStringsValid(tool) {
				return false
			}
		}
	case model.ToolIdentity:
		return utf8.ValidString(v.Name) && utf8.ValidString(v.Path) && utf8.ValidString(v.Version) && utf8.ValidString(v.SHA256)
	case model.Coverage:
		return utf8.ValidString(v.ScopeID) && utf8.ValidString(string(v.Fact)) && utf8.ValidString(string(v.State)) && utf8.ValidString(v.Reason)
	case struct {
		ScopeID string
		Tool    model.ToolIdentity
	}:
		return utf8.ValidString(v.ScopeID) && allStringsValid(v.Tool)
	}
	return true
}

func cloneRequest(req model.Request) model.Request {
	req.Scopes = cloneScopes(req.Scopes)
	req.Provenance = cloneProvenance(req.Provenance)
	return req
}

func cloneScopes(scopes []model.Scope) []model.Scope {
	out := append([]model.Scope(nil), scopes...)
	for i := range out {
		out[i].Build.Environment = cloneStringMap(out[i].Build.Environment)
		out[i].Build.Arguments = append([]string(nil), out[i].Build.Arguments...)
		out[i].Build.PackagePatterns = append([]string(nil), out[i].Build.PackagePatterns...)
		out[i].Build.IncludePaths = append([]string(nil), out[i].Build.IncludePaths...)
		out[i].Build.Defines = cloneStringMap(out[i].Build.Defines)
		out[i].Build.Features = append([]string(nil), out[i].Build.Features...)
		out[i].Build.Options = cloneStringMap(out[i].Build.Options)
	}
	return out
}

func cloneProvenance(values map[string]model.Provenance) map[string]model.Provenance {
	out := make(map[string]model.Provenance, len(values))
	for key, value := range values {
		value.Tools = append([]model.ToolIdentity(nil), value.Tools...)
		value.Scope = cloneScopes([]model.Scope{value.Scope})[0]
		out[key] = value
	}
	return out
}

func cloneUsedTools(values map[string][]model.ToolIdentity) map[string][]model.ToolIdentity {
	out := make(map[string][]model.ToolIdentity, len(values))
	for key, tools := range values {
		out[key] = append([]model.ToolIdentity(nil), tools...)
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func durableIdentity(value model.Identity) model.Identity {
	value.SnapshotRev = 0
	return value
}

var _ model.Sink = (*Sink)(nil)
