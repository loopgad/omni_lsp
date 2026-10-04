package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
)

// Reader iterates a verified generation one typed segment at a time. OpenReader
// performs a bounded validation pass, including external-sort symbol checks,
// before returning any batch to a query caller.
type Reader struct {
	view     persistent.GenerationView
	metadata Metadata
	scopes   map[string]model.Scope
	index    int
	closed   bool
}

// OpenReader validates metadata and all typed segments without retaining fact
// batches, then returns an iterator over the immutable generation.
func OpenReader(ctx context.Context, view persistent.GenerationView) (*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(view.Segments) == 0 {
		return nil, &NotSemanticError{Reason: "generation has no segments"}
	}
	segments := append([]persistent.SegmentRef(nil), view.Segments...)
	for _, ref := range segments {
		if ref.Len <= 0 || ref.Len > MaxSegmentBytes+4096 {
			return nil, fmt.Errorf("%w: stored segment %s length %d", ErrBounds, ref.ID, ref.Len)
		}
	}
	metadataID := segments[len(segments)-1].ID
	payload, err := readSegment(view, metadataID)
	if err != nil {
		return nil, err
	}
	wire, err := decodeWire(payload, metadataID)
	if err != nil {
		return nil, err
	}
	if wire.Kind != "metadata" || len(wire.Metadata) == 0 || len(wire.Records) != 0 {
		return nil, fmt.Errorf("%w: generation has no terminal metadata segment", ErrMalformedPayload)
	}
	var metadata Metadata
	if err := decodeStrict(wire.Metadata, &metadata); err != nil {
		return nil, fmt.Errorf("%w: metadata segment %s: %v", ErrMalformedPayload, metadataID, err)
	}
	scopes, err := validateMetadata(metadata)
	if err != nil {
		return nil, err
	}
	ledger, err := newIDLedger()
	if err != nil {
		return nil, err
	}
	defer ledger.Close()

	reader := &Reader{view: view, metadata: cloneMetadata(metadata), scopes: scopes}
	for i := 0; i < len(segments)-1; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := reader.readBatch(ctx, i)
		if err != nil {
			return nil, err
		}
		if err := addBatchIDs(ledger, batch); err != nil {
			return nil, err
		}
	}
	if err := ledger.Validate(ctx, metadata.Coverage); err != nil {
		return nil, err
	}
	reader.index = 0
	return reader, nil
}

// Metadata returns a deep copy of the generation's identity and extraction
// report so callers cannot mutate the reader's validation state.
func (r *Reader) Metadata() Metadata {
	if r == nil {
		return Metadata{}
	}
	return cloneMetadata(r.metadata)
}

// Next returns one decoded typed batch. It returns io.EOF after the final data
// segment; the terminal metadata segment is available through Metadata().
func (r *Reader) Next(ctx context.Context) (Batch, error) {
	if r == nil || r.closed {
		return Batch{}, errors.New("semantic: reader is closed")
	}
	if err := ctx.Err(); err != nil {
		return Batch{}, err
	}
	if r.index >= len(r.view.Segments)-1 {
		return Batch{}, io.EOF
	}
	batch, err := r.readBatch(ctx, r.index)
	if err != nil {
		return Batch{}, err
	}
	r.index++
	return batch, nil
}

// Rewind resets the iterator to the first data segment after OpenReader has
// validated the immutable generation. It intentionally does not repeat the
// validation pass; callers must continue using the same GenerationView.
func (r *Reader) Rewind() error {
	if r == nil || r.closed {
		return errors.New("semantic: reader is closed")
	}
	r.index = 0
	return nil
}

// Close prevents further reads. GenerationView itself owns no open resources.
func (r *Reader) Close() error {
	if r != nil {
		r.closed = true
	}
	return nil
}

func (r *Reader) readBatch(ctx context.Context, index int) (Batch, error) {
	if err := ctx.Err(); err != nil {
		return Batch{}, err
	}
	ref := r.view.Segments[index]
	payload, err := readSegment(r.view, ref.ID)
	if err != nil {
		return Batch{}, err
	}
	wire, err := decodeWire(payload, ref.ID)
	if err != nil {
		return Batch{}, err
	}
	if wire.Kind == "metadata" || len(wire.Metadata) != 0 {
		return Batch{}, fmt.Errorf("%w: unexpected metadata segment %s", ErrMalformedPayload, ref.ID)
	}
	if len(wire.Records) == 0 || string(wire.Records) == "null" {
		return Batch{}, fmt.Errorf("%w: segment %s has no records", ErrMalformedPayload, ref.ID)
	}
	batch, err := decodeBatch(wire.Kind, wire.Records, ref.ID)
	if err != nil {
		return Batch{}, err
	}
	if err := r.validateBatch(batch); err != nil {
		return Batch{}, fmt.Errorf("%w: segment %s: %v", ErrMalformedPayload, ref.ID, err)
	}
	return batch, nil
}

func readSegment(view persistent.GenerationView, id persistent.SegmentID) ([]byte, error) {
	payload, err := view.ReadSegment(id)
	if err != nil {
		return nil, fmt.Errorf("%w: segment %s: %w", ErrMalformedPayload, id, err)
	}
	if len(payload) > MaxSegmentBytes {
		return nil, fmt.Errorf("%w: segment %s is %d bytes", ErrBounds, id, len(payload))
	}
	return payload, nil
}

func decodeBatch(kind string, raw json.RawMessage, id persistent.SegmentID) (Batch, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		return Batch{}, fmt.Errorf("%w: segment %s records: %v", ErrMalformedPayload, id, err)
	}
	if records == nil {
		return Batch{}, fmt.Errorf("%w: segment %s records are not an array", ErrMalformedPayload, id)
	}
	batch := Batch{Kind: BatchKind(kind)}
	switch batch.Kind {
	case BatchSymbols:
		batch.Symbols = make([]model.Symbol, 0, len(records))
		for _, rawRecord := range records {
			var value model.Symbol
			if err := decodeRecord(rawRecord, &value); err != nil {
				return Batch{}, fmt.Errorf("%w: symbol in segment %s: %v", ErrMalformedPayload, id, err)
			}
			batch.Symbols = append(batch.Symbols, value)
		}
	case BatchOccurrences:
		batch.Occurrences = make([]model.Occurrence, 0, len(records))
		for _, rawRecord := range records {
			var value model.Occurrence
			if err := decodeRecord(rawRecord, &value); err != nil {
				return Batch{}, fmt.Errorf("%w: occurrence in segment %s: %v", ErrMalformedPayload, id, err)
			}
			batch.Occurrences = append(batch.Occurrences, value)
		}
	case BatchEdges:
		batch.Edges = make([]model.Edge, 0, len(records))
		for _, rawRecord := range records {
			var value model.Edge
			if err := decodeRecord(rawRecord, &value); err != nil {
				return Batch{}, fmt.Errorf("%w: edge in segment %s: %v", ErrMalformedPayload, id, err)
			}
			batch.Edges = append(batch.Edges, value)
		}
	default:
		return Batch{}, fmt.Errorf("%w: unknown segment kind %q", ErrMalformedPayload, kind)
	}
	return batch, nil
}

func decodeRecord(raw json.RawMessage, dst any) error {
	if len(raw) > MaxRecordBytes {
		return fmt.Errorf("%w: record is %d bytes", ErrBounds, len(raw))
	}
	if err := decodeStrict(raw, dst); err != nil {
		return err
	}
	return nil
}

func (r *Reader) validateBatch(batch Batch) error {
	validateScope := func(scopeID string) (model.Scope, error) {
		if !validIdentifier(scopeID) || scopeID == "" {
			return model.Scope{}, ErrInvalidScope
		}
		scope, ok := r.scopes[scopeID]
		if !ok {
			return model.Scope{}, fmt.Errorf("%w: unknown scope %q", ErrInvalidScope, scopeID)
		}
		if !model.HasPinnedToolUsage(r.metadata.Provenance[scopeID].Tools, r.metadata.UsedTools[scopeID]) {
			return model.Scope{}, model.ErrInvalidProvenance
		}
		return scope, nil
	}
	validateSource := func(scope model.Scope, uri string, hash string) error {
		if !uriWithinRoot(scope.RootURI, uri) || !validContentHash(identityHash(hash)) {
			return ErrInvalidSource
		}
		return nil
	}
	switch batch.Kind {
	case BatchSymbols:
		for _, v := range batch.Symbols {
			if _, err := validateScope(v.ScopeID); err != nil {
				return err
			}
			if v.ID == "" || !validIdentifier(string(v.ID)) || !validString(v.Name) || !validString(v.Kind) || !validString(v.Signature) {
				return ErrMalformedPayload
			}
		}
	case BatchOccurrences:
		for _, v := range batch.Occurrences {
			scope, err := validateScope(v.ScopeID)
			if err != nil {
				return err
			}
			if v.SymbolID == "" || !validIdentifier(string(v.SymbolID)) || !validString(v.Role) || !validPosition(v.Range) {
				return ErrMalformedPayload
			}
			if v.BuildContext == "" || v.BuildContext != scope.BuildContext {
				return ErrBuildContext
			}
			if err := validateSource(scope, v.URI, string(v.SourceHash)); err != nil {
				return err
			}
		}
	case BatchEdges:
		for _, v := range batch.Edges {
			scope, err := validateScope(v.ScopeID)
			if err != nil {
				return err
			}
			if v.From == "" || v.To == "" || !validIdentifier(string(v.From)) || !validIdentifier(string(v.To)) ||
				!validString(string(v.Kind)) || !validPosition(v.Range) || !validString(v.SourceURI) {
				return ErrMalformedPayload
			}
			if _, ok := edgeFact(v.Kind); !ok {
				return ErrMalformedPayload
			}
			if v.BuildContext == "" || v.BuildContext != scope.BuildContext {
				return ErrBuildContext
			}
			if err := validateSource(scope, v.SourceURI, string(v.SourceHash)); err != nil {
				return err
			}
		}
	}
	return nil
}

func addBatchIDs(ledger *idLedger, batch Batch) error {
	switch batch.Kind {
	case BatchSymbols:
		for _, value := range batch.Symbols {
			if err := ledger.Add(value.ScopeID, string(value.ID), idDeclared, ""); err != nil {
				return err
			}
		}
	case BatchOccurrences:
		for _, value := range batch.Occurrences {
			if err := ledger.Add(value.ScopeID, string(value.SymbolID), idReferenced, occurrenceFact(value.Role)); err != nil {
				return err
			}
		}
	case BatchEdges:
		for _, value := range batch.Edges {
			fact, ok := edgeFact(value.Kind)
			if !ok {
				return ErrMalformedPayload
			}
			if err := ledger.Add(value.ScopeID, string(value.From), idReferenced, fact); err != nil {
				return err
			}
			if err := ledger.Add(value.ScopeID, string(value.To), idReferenced, fact); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMetadata(meta Metadata) (map[string]model.Scope, error) {
	if meta.SchemaVersion != SchemaVersion || meta.Identity.Workspace == "" || meta.Identity.SnapshotRev != 0 ||
		!validString(string(meta.Identity.Workspace)) ||
		!validContentHash(meta.DiskDigest) || meta.DiskDigest != meta.Identity.DiskDigest {
		return nil, fmt.Errorf("%w: invalid semantic metadata identity", ErrMalformedPayload)
	}
	if len(meta.Scopes) == 0 {
		return nil, ErrInvalidScope
	}
	if err := validateMetadataRecords(meta); err != nil {
		return nil, err
	}
	scopes := make(map[string]model.Scope, len(meta.Scopes))
	for _, scope := range meta.Scopes {
		if !validScope(scope) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidScope, scope.ID)
		}
		if _, exists := scopes[scope.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate scope %q", ErrInvalidScope, scope.ID)
		}
		provenance, ok := meta.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != meta.Identity ||
			!reflect.DeepEqual(provenance.Scope, scope) || provenance.Extractor == "" || provenance.ExtractorVer == "" ||
			provenance.Toolchain == "" || provenance.Backend.Name == "" || provenance.Backend.Language != scope.Language ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return nil, fmt.Errorf("%w: scope %q provenance or BuildContext", model.ErrInvalidProvenance, scope.ID)
		}
		for _, tool := range provenance.Tools {
			if err := validateTool(tool); err != nil {
				return nil, err
			}
		}
		scopes[scope.ID] = scope
	}
	if len(meta.Provenance) != len(scopes) {
		return nil, model.ErrInvalidProvenance
	}
	seenCoverage := make(map[string]bool, len(scopes)*len(model.RequiredFactKinds))
	for _, item := range meta.Coverage {
		if _, ok := scopes[item.ScopeID]; !ok || !validFact(item.Fact) || !validCompleteness(item.State) ||
			(item.State != model.Complete && item.Reason == "") {
			return nil, model.ErrInvalidCoverage
		}
		key := item.ScopeID + "\x00" + string(item.Fact)
		if seenCoverage[key] {
			return nil, model.ErrDuplicateCoverage
		}
		seenCoverage[key] = true
	}
	for scopeID := range scopes {
		for _, fact := range model.RequiredFactKinds {
			if !seenCoverage[scopeID+"\x00"+string(fact)] {
				return nil, model.ErrMissingCoverage
			}
		}
	}
	for scopeID, tools := range meta.UsedTools {
		if _, ok := scopes[scopeID]; !ok {
			return nil, model.ErrInvalidProvenance
		}
		for _, tool := range tools {
			if err := validateTool(tool); err != nil {
				return nil, err
			}
		}
	}
	for _, scope := range meta.Scopes {
		if model.ScopeHasNoAttestedFacts(scope.ID, meta.Coverage) {
			continue
		}
		if !model.HasPinnedToolUsage(meta.Provenance[scope.ID].Tools, meta.UsedTools[scope.ID]) {
			return nil, model.ErrInvalidProvenance
		}
	}
	return scopes, nil
}

func validateTool(tool model.ToolIdentity) error {
	if tool.Name == "" || tool.Version == "" || !validToolPath(tool.Path) || !validSHA256Hex(tool.SHA256) {
		return model.ErrInvalidProvenance
	}
	return nil
}

func validToolPath(value string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && (value[2] == '/' || value[2] == '\\') || strings.HasPrefix(value, `\\`)
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func validFact(kind model.FactKind) bool {
	for _, required := range model.RequiredFactKinds {
		if kind == required {
			return true
		}
	}
	return false
}

func validCompleteness(state model.Completeness) bool {
	return state == model.Complete || state == model.IncompleteKnownSubset || state == model.Unknown || state == model.Unavailable
}

func cloneMetadata(meta Metadata) Metadata {
	meta.Scopes = cloneScopes(meta.Scopes)
	meta.Provenance = cloneProvenance(meta.Provenance)
	meta.Coverage = append([]model.Coverage(nil), meta.Coverage...)
	meta.UsedTools = cloneUsedTools(meta.UsedTools)
	return meta
}

// identityHash keeps source-hash validation in the same canonical SHA-256
// format used by immutable workspace views.
func identityHash(value string) identity.ContentHash { return identity.ContentHash(value) }
