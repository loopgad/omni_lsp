package corpus

// §S21 accuracy verification (goal.md 行 4474-4487): every response produced
// while serving the qualified corpus is inspected and sorted into exactly the
// six release-blocking error buckets. The qualified-corpus goal is that all
// six counters stay zero:
//
//	Wrong Edit Rate            = 0
//	Stale Edit Applied         = 0
//	Wrong-file Location        = 0
//	Position Mapping Error     = 0
//	Protocol-invalid Response  = 0
//	Snapshot Mixing            = 0
//
// The classifier lives in this non-test file so every language corpus runner
// (test/corpus and internal/languages/golang) shares one implementation.

import (
	"fmt"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/position"
)

// ErrorBuckets aggregates §S21 zero-error classification counters.
type ErrorBuckets struct {
	WrongEdit               int64
	StaleEdit               int64
	WrongFileLocation       int64
	PositionMappingError    int64
	ProtocolInvalidResponse int64
	SnapshotMixing          int64
}

func (b ErrorBuckets) String() string {
	rows := []struct {
		name string
		n    int64
	}{
		{"Wrong Edit Rate", b.WrongEdit},
		{"Stale Edit Applied", b.StaleEdit},
		{"Wrong-file Location", b.WrongFileLocation},
		{"Position Mapping Error", b.PositionMappingError},
		{"Protocol-invalid Response", b.ProtocolInvalidResponse},
		{"Snapshot Mixing", b.SnapshotMixing},
	}
	var sb strings.Builder
	sb.WriteString("§S21 error buckets\n")
	sb.WriteString("Bucket                       Count\n")
	sb.WriteString("---------------------------  -----\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-27s %5d\n", r.name, r.n)
	}
	return sb.String()
}

func (b *ErrorBuckets) add(bucket string) {
	switch bucket {
	case "wrongEdit":
		b.WrongEdit++
	case "staleEdit":
		b.StaleEdit++
	case "wrongFileLocation":
		b.WrongFileLocation++
	case "positionMappingError":
		b.PositionMappingError++
	case "protocolInvalidResponse":
		b.ProtocolInvalidResponse++
	case "snapshotMixing":
		b.SnapshotMixing++
	default:
		panic("unknown bucket " + bucket)
	}
}

// Total is the sum across all six buckets; §S21 requires it to be zero.
func (b *ErrorBuckets) Total() int64 {
	return b.WrongEdit + b.StaleEdit + b.WrongFileLocation +
		b.PositionMappingError + b.ProtocolInvalidResponse + b.SnapshotMixing
}

// LSPResponse captures the classifiable surface of an LSP JSON-RPC response
// envelope plus its semantic payload, independent of transport. The typed
// bridge results are projected onto it so one classifier serves every
// feature.
type LSPResponse struct {
	Feature string // hover | definition | references | rename
	Err     string // transport/dispatch failure; "" = protocol-clean
	Status  identity.ResultStatus

	URIs        []string          // result.uri / location[].uri / edit URIs
	Ranges      []languages.Range // ranges reported against the request document
	EditNewText []string          // parallel to Ranges for rename edits

	Rev   uint64 // snapshot revision the response claims (evidence §B4)
	Epoch uint64 // backend epoch the response claims (evidence §B4)
}

// ClassifyRequest carries everything a response must be consistent with.
type ClassifyRequest struct {
	URI     string
	Content []byte
	Symbol  string // identifier expected under any reported range
	NewName string // rename target text, "" for read-only features
	Rev     uint64 // current snapshot revision
	Epoch   uint64 // current backend epoch baseline
}

// ClassifyResponse sorts one response into §S21 buckets and returns the names
// of every bucket it hit (empty slice = clean). Pure function, no state.
func ClassifyResponse(req ClassifyRequest, resp LSPResponse) []string {
	var hits []string
	hit := func(b string) { hits = append(hits, b) }

	// Protocol-invalid: dispatch failed or an illegal result status shape
	// arrived. Unknown/Unavailable are legal refusals (§S8), not errors here.
	if resp.Err != "" || resp.Status > identity.ResultUnavailable {
		hit("protocolInvalidResponse")
	}

	// Wrong-file location: every returned URI must name the requested doc.
	for _, u := range resp.URIs {
		if u != "" && u != req.URI {
			hit("wrongFileLocation")
			break
		}
	}

	// Position mapping + wrong-edit: each reported range must round-trip
	// through the UTF-16 engine and cover exactly the target symbol.
	mappingFailed := false
	for i, r := range resp.Ranges {
		if !roundTrips(req.Content, r) {
			mappingFailed = true
			continue // extraction impossible — wrong-edit check not applicable
		}
		text, _ := rangeText(req.Content, r)
		if text != req.Symbol {
			hit("wrongEdit")
		} else if resp.Feature == "rename" && len(resp.EditNewText) > i &&
			resp.EditNewText[i] != req.NewName {
			hit("wrongEdit") // right range, renamed to something else
		}
	}
	if mappingFailed {
		hit("positionMappingError")
	}

	// Stale edit + snapshot mixing share a root cause (response claims a
	// revision or backend generation that no longer matches), so both
	// buckets record it (goal.md S21 rows 2 and 6).
	if resp.Rev != req.Rev || resp.Epoch != req.Epoch {
		hit("staleEdit")
		hit("snapshotMixing")
	}
	return hits
}

// Record classifies resp and increments every bucket it hit.
func (b *ErrorBuckets) Record(req ClassifyRequest, resp LSPResponse) []string {
	hits := ClassifyResponse(req, resp)
	for _, h := range hits {
		b.add(h)
	}
	return hits
}

// roundTrips reports whether range start AND end survive the UTF-16
// offset→line:char→offset mapping unchanged.
func roundTrips(content []byte, r languages.Range) bool {
	return mapsBack(content, r.StartLine, r.StartCharacter) &&
		mapsBack(content, r.EndLine, r.EndCharacter)
}

func mapsBack(content []byte, line, char uint32) bool {
	off, err := position.OffsetOfLineChar(content, line, char)
	if err != nil {
		return false
	}
	l, c, err := position.LineCharAt(content, off)
	return err == nil && l == line && c == char
}

// rangeText extracts the bytes covered by an LSP range via the UTF-16 engine.
func rangeText(content []byte, r languages.Range) (string, bool) {
	so, err := position.OffsetOfLineChar(content, r.StartLine, r.StartCharacter)
	if err != nil {
		return "", false
	}
	eo, err := position.OffsetOfLineChar(content, r.EndLine, r.EndCharacter)
	if err != nil || eo < so || eo > uint32(len(content)) {
		return "", false
	}
	return string(content[so:eo]), true
}

// --- projections from typed bridge results onto LSPResponse ---

func evidenceStamp(ev []identity.Evidence) (rev, epoch uint64) {
	if len(ev) == 0 {
		return 0, 0
	}
	return uint64(ev[0].Snapshot.Revision), uint64(ev[0].BackendEpoch)
}

// HoverEnvelope projects a typed hover result onto the classifiable shape.
func HoverEnvelope(res identity.SemanticResult[*languages.HoverResult], err error) LSPResponse {
	resp := LSPResponse{Feature: "hover", Err: errText(err), Status: res.Status}
	resp.Rev, resp.Epoch = evidenceStamp(res.Evidence)
	if res.Value != nil {
		if res.Value.Range != nil {
			resp.Ranges = append(resp.Ranges, *res.Value.Range)
		}
	}
	return resp
}

// LocationsEnvelope projects definition/references results onto LSPResponse.
func LocationsEnvelope(feature string, res identity.SemanticResult[[]languages.Location], err error) LSPResponse {
	resp := LSPResponse{Feature: feature, Err: errText(err), Status: res.Status}
	resp.Rev, resp.Epoch = evidenceStamp(res.Evidence)
	for _, loc := range res.Value {
		if loc.URI != "" {
			resp.URIs = append(resp.URIs, loc.URI)
		}
		resp.Ranges = append(resp.Ranges, loc.Range)
	}
	return resp
}

// RenameEnvelope projects a validated rename edit onto LSPResponse.
func RenameEnvelope(res identity.SemanticResult[languages.ValidatedEdit], err error) LSPResponse {
	resp := LSPResponse{Feature: "rename", Err: errText(err), Status: res.Status}
	resp.Rev, resp.Epoch = evidenceStamp(res.Evidence)
	for _, e := range res.Value.Edits {
		if e.URI != "" {
			resp.URIs = append(resp.URIs, e.URI)
		}
		resp.Ranges = append(resp.Ranges, languages.Range{
			StartLine: e.StartLine, StartCharacter: e.StartChar,
			EndLine: e.EndLine, EndCharacter: e.EndChar,
		})
		resp.EditNewText = append(resp.EditNewText, e.NewText)
	}
	return resp
}

func errText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
