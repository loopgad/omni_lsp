package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// Pin for verifierB-r23b entry 2: the C/C++ extractor emits a "declaration"
// occurrence for every non-definition cursor and for macro definitions
// (extractors/clang/helper.cpp.txt), and the semantic sink persists the role
// (occurrenceFact maps declaration → FactDeclaration) — but both wire
// exporters only knew reference/definition, so a real workspace's generation
// was persistable yet unexportable through ALL FOUR export paths. SCIP has no
// declaration bit (SymbolRoles is Definition-or-nothing) and the omnilsp LSIF
// subset likewise only distinguishes definitions from references, so a
// declaration exports as the reference shape; the lost declaration/reference
// distinction is registered in LossyNotes (§L14) and the re-import round trip
// pins the mapped role.
func TestDeclarationOccurrencesExportThroughAllFourPaths(t *testing.T) {
	ctx := context.Background()
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	declRange := model.Position{StartLine: 1, StartChar: 3, EndLine: 1, EndChar: 6}
	occurrences := []model.Occurrence{
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}, Role: "definition", SourceHash: hashes["file:///repo/main.go"], BuildContext: scope.BuildContext},
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/refs.go", Range: declRange, Role: "declaration", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext},
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/refs.go", Range: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 2}, Role: "reference", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext},
	}
	store := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function", Signature: "func Foo()"}},
		occurrences, nil,
	)
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	// The declaration occurrence is persisted and readable back: the sink
	// accepted it, so the exporters must not treat it as unrepresentable.
	sawDeclaration := false
	for {
		batch, err := reader.Next(ctx)
		if batch.Kind == semantic.BatchOccurrences {
			for _, occ := range batch.Occurrences {
				sawDeclaration = sawDeclaration || occ.Role == "declaration"
			}
		}
		if err != nil {
			break
		}
	}
	if !sawDeclaration {
		t.Fatal("fixture precondition: persisted generation holds no declaration occurrence")
	}
	if err := reader.Rewind(); err != nil {
		t.Fatal(err)
	}

	scipData, err := ExportSCIPFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportSCIPFromSemantic: %v", err)
	}
	if err := reader.Rewind(); err != nil {
		t.Fatal(err)
	}
	scipLossyData, _, err := ExportSCIPSemanticScopeWithLosses(ctx, reader, scope.ID)
	if err != nil {
		t.Fatalf("ExportSCIPSemanticScopeWithLosses: %v", err)
	}
	if err := reader.Rewind(); err != nil {
		t.Fatal(err)
	}
	lsifData, err := ExportLSIFFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportLSIFFromSemantic: %v", err)
	}
	if err := reader.Rewind(); err != nil {
		t.Fatal(err)
	}
	lsifLossyData, _, err := ExportLSIFFromSemanticWithLosses(ctx, reader)
	if err != nil {
		t.Fatalf("ExportLSIFFromSemanticWithLosses: %v", err)
	}
	if len(scipData) == 0 || len(scipLossyData) == 0 || len(lsifData) == 0 || len(lsifLossyData) == 0 {
		t.Fatal("an export path produced 0 bytes")
	}

	// SCIP wire shape: the declaration occurrence is present without the
	// Definition bit (SCIP cannot carry a declaration role).
	var index scip.Index
	if err := proto.Unmarshal(scipData, &index); err != nil {
		t.Fatalf("decode exported SCIP: %v", err)
	}
	sawDeclShape := false
	for _, doc := range index.GetDocuments() {
		for _, occ := range doc.GetOccurrences() {
			if occ.GetSymbol() != string(foo) || len(occ.GetRange()) != 4 {
				continue
			}
			if occ.GetRange()[0] != int32(declRange.StartLine) || occ.GetRange()[1] != int32(declRange.StartChar) {
				continue
			}
			if occ.GetSymbolRoles()&int32(scip.SymbolRole_Definition) != 0 {
				t.Fatal("declaration occurrence exported with the Definition bit")
			}
			sawDeclShape = true
		}
	}
	if !sawDeclShape {
		t.Fatal("declaration occurrence missing from the SCIP output")
	}

	// LSIF wire shape: the declaration occurrence is a referenceResults item,
	// not a definitions item (the omnilsp LSIF subset has no declaration slot).
	rangeByStart := map[string]int{}
	itemProperty := map[int]string{}
	for _, line := range bytes.Split(lsifData, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var node lsifNodeRaw
		if err := json.Unmarshal(line, &node); err != nil {
			t.Fatalf("decode LSIF line: %v", err)
		}
		switch node.Label {
		case "range":
			var vertex lsifVertex
			if err := json.Unmarshal(line, &vertex); err != nil {
				t.Fatalf("decode LSIF range: %v", err)
			}
			if vertex.Start != nil {
				rangeByStart[lsifPositionKey(*vertex.Start)] = node.ID
			}
		case "item":
			var edge lsifEdge
			if err := json.Unmarshal(line, &edge); err != nil {
				t.Fatalf("decode LSIF item edge: %v", err)
			}
			itemProperty[edge.InV] = edge.Property
		}
	}
	declRangeID, ok := rangeByStart[lsifPositionKey(lsifPosition{Line: int32(declRange.StartLine), Character: int32(declRange.StartChar)})]
	if !ok {
		t.Fatal("declaration occurrence range missing from the LSIF output")
	}
	if got := itemProperty[declRangeID]; got != "referenceResults" {
		t.Fatalf("declaration occurrence exported as LSIF item property %q, want referenceResults", got)
	}

	// §L14: the declaration → reference-shape mapping is a lossy mapping and
	// must be documented.
	if !strings.Contains(strings.Join(LossyNotes(), "\n"), "declaration") {
		t.Fatal("LossyNotes does not document the declaration occurrence mapping")
	}

	// Re-import round trip: the declaration occurrence survives as a
	// reference-role occurrence (the wire shape has no declaration slot).
	reimported, reimportedSink, reimportedBuild := newSCIPRoundTripSink(t, req)
	importedReport, err := ImportSCIPToModel(ctx, scipData, req, reimportedSink)
	if err != nil {
		_ = reimportedBuild.Abort()
		t.Fatalf("ImportSCIPToModel: %v", err)
	}
	if err := reimportedSink.Finalize(ctx, importedReport); err != nil {
		_ = reimportedBuild.Abort()
		t.Fatalf("finalize re-imported generation: %v", err)
	}
	if err := reimportedBuild.Commit(ctx); err != nil {
		t.Fatalf("commit re-imported generation: %v", err)
	}
	reimportedView, err := reimported.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reimportedReader, err := semantic.OpenReader(ctx, reimportedView)
	if err != nil {
		t.Fatal(err)
	}
	defer reimportedReader.Close()
	sawReferenceAtDeclRange := false
	for {
		batch, err := reimportedReader.Next(ctx)
		if batch.Kind == semantic.BatchOccurrences {
			for _, occ := range batch.Occurrences {
				if occ.SymbolID == foo && occ.URI == "file:///repo/refs.go" &&
					occ.Range.StartLine == declRange.StartLine && occ.Range.StartChar == declRange.StartChar {
					if occ.Role != "reference" {
						t.Fatalf("re-imported declaration occurrence has role %q, want the documented reference mapping", occ.Role)
					}
					sawReferenceAtDeclRange = true
				}
			}
		}
		if err != nil {
			break
		}
	}
	if !sawReferenceAtDeclRange {
		t.Fatal("re-imported generation lost the declaration occurrence entirely")
	}
}

func lsifPositionKey(p lsifPosition) string {
	return fmt.Sprintf("%d:%d", p.Line, p.Character)
}
