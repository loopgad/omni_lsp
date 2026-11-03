package interop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// Pin for round20/bugB bugs 1/3: LSIF metaData.positionEncoding was dropped
// on the floor — the import-side union struct never carried the field, so
// ImportLSIFToModel read every column as UTF-16 and ConvertLSIFToSCIP
// hard-declared TextEncoding_UTF16 no matter what the stream declared. A
// utf-8 (or utf-32) producer stream thus silently mis-decodes every position
// after a multi-byte character (validateUTF16Ranges only rejects columns
// beyond the line's UTF-16 length), while the SCIP importer handles the same
// declaration correctly (convert via utf8RangesToUTF16, reject undeclared).

// lsifEncodingFixture builds a one-document LSIF stream with the given
// positionEncoding and a model request over a view holding the text.
func lsifEncodingFixture(t *testing.T, encoding, text string, start, end int32) (model.Request, []byte) {
	t.Helper()
	hash := sha256.Sum256([]byte(text))
	file := model.File{
		URI: "file:///repo/main.go", LanguageID: "go",
		Size: int64(len(text)), SHA256: identity.ContentHash("sha256:" + hex.EncodeToString(hash[:])),
	}
	id := model.Identity{
		Workspace: "workspace-lsif-encoding", DiskDigest: identity.ContentHash("sha256:" + hex.EncodeToString(hash[:1])),
		Repository: "https://code.example/omnilsp", Revision: "0123456789abcdef", SnapshotRev: 3,
	}
	view := scipModelView{id: id, file: file, text: text}
	scope := model.Scope{ID: "go:module:repo", Language: "go", RootURI: "file:///repo", Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "lsif-importer", "1", "test-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: id, Scope: scope,
		Extractor: "lsif-importer", ExtractorVer: "1", Toolchain: "test-toolchain",
		Backend: identity.BackendID{Language: scope.Language, Name: "lsif"},
	}
	req := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}

	meta := `{"id":1,"label":"metaData","version":"0.5.0","projectRoot":"file:///repo"`
	if encoding != "" {
		meta += `,"positionEncoding":"` + encoding + `"`
	}
	meta += `}`
	stream := strings.Join([]string{
		meta,
		`{"id":2,"label":"document","uri":"file:///repo/main.go","languageId":"go"}`,
		`{"id":3,"label":"resultSet","symbol":"scip-go repo main#Foo."}`,
		`{"id":4,"label":"range","start":{"line":0,"character":` + strconv.Itoa(int(start)) + `},"end":{"line":0,"character":` + strconv.Itoa(int(end)) + `}}`,
		`{"id":5,"label":"contains","outV":2,"inVs":[4]}`,
		`{"id":6,"label":"next","outV":4,"inV":3}`,
		`{"id":7,"label":"item","outV":3,"inV":4}`,
	}, "\n")
	return req, []byte(stream)
}

func importLSIFStream(t *testing.T, req model.Request, stream []byte) []model.Occurrence {
	t.Helper()
	sink := &scipModelSink{}
	if _, err := ImportLSIFToModel(context.Background(), stream, req, sink); err != nil {
		t.Fatalf("ImportLSIFToModel: %v", err)
	}
	return sink.occurrences
}

func TestLSIFImportHonoursUTF8DeclaredColumns(t *testing.T) {
	// "héllo world": 'é' is 2 UTF-8 bytes / 1 UTF-16 unit, so byte column 6
	// is UTF-16 column 5 and byte column 7 is UTF-16 column 6.
	req, stream := lsifEncodingFixture(t, "utf-8", "héllo world\n", 6, 7)
	occs := importLSIFStream(t, req, stream)
	if len(occs) != 1 {
		t.Fatalf("imported %d occurrences, want 1", len(occs))
	}
	got := occs[0].Range
	if got.StartChar != 5 || got.EndChar != 6 {
		t.Fatalf("utf-8 byte columns imported verbatim as UTF-16: got %+v, want StartChar=5 EndChar=6", got)
	}
}

func TestLSIFImportHonoursUTF32DeclaredColumns(t *testing.T) {
	// "a𝐛x": 𝐛 (U+1D41B) is 1 code point / 2 UTF-16 units / 4 UTF-8 bytes,
	// so code point column 2 is UTF-16 column 3.
	req, stream := lsifEncodingFixture(t, "utf-32", "a𝐛x\n", 2, 3)
	occs := importLSIFStream(t, req, stream)
	if len(occs) != 1 {
		t.Fatalf("imported %d occurrences, want 1", len(occs))
	}
	got := occs[0].Range
	if got.StartChar != 3 || got.EndChar != 4 {
		t.Fatalf("utf-32 code point columns imported verbatim as UTF-16: got %+v, want StartChar=3 EndChar=4", got)
	}
}

func TestLSIFImportKeepsUTF16DeclaredColumns(t *testing.T) {
	req, stream := lsifEncodingFixture(t, "utf-16", "héllo world\n", 5, 6)
	occs := importLSIFStream(t, req, stream)
	if len(occs) != 1 {
		t.Fatalf("imported %d occurrences, want 1", len(occs))
	}
	if got := occs[0].Range; got.StartChar != 5 || got.EndChar != 6 {
		t.Fatalf("utf-16 columns must pass through verbatim, got %+v", got)
	}
}

func TestLSIFImportRejectsUndeclaredEncoding(t *testing.T) {
	req, stream := lsifEncodingFixture(t, "", "héllo world\n", 6, 7)
	sink := &scipModelSink{}
	if _, err := ImportLSIFToModel(context.Background(), stream, req, sink); err == nil {
		t.Fatal("a stream without a positionEncoding declaration must be rejected, not silently read as UTF-16")
	}
}

func TestLSIFImportRejectsUnknownEncoding(t *testing.T) {
	req, stream := lsifEncodingFixture(t, "utf-7", "héllo world\n", 6, 7)
	sink := &scipModelSink{}
	if _, err := ImportLSIFToModel(context.Background(), stream, req, sink); err == nil {
		t.Fatal("an unknown positionEncoding declaration must be rejected")
	}
}

func TestConvertLSIFToSCIPDeclaresActualEncoding(t *testing.T) {
	_, stream := lsifEncodingFixture(t, "utf-8", "héllo world\n", 6, 7)
	data, err := ConvertLSIFToSCIP(stream)
	if err != nil {
		t.Fatalf("ConvertLSIFToSCIP: %v", err)
	}
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.GetMetadata().GetTextDocumentEncoding() != scip.TextEncoding_UTF8 {
		t.Fatalf("utf-8 LSIF stream converted with a lying %s declaration, want UTF-8",
			index.GetMetadata().GetTextDocumentEncoding())
	}
}

func TestConvertLSIFToSCIPKeepsUTF16Declaration(t *testing.T) {
	_, stream := lsifEncodingFixture(t, "utf-16", "héllo world\n", 5, 6)
	data, err := ConvertLSIFToSCIP(stream)
	if err != nil {
		t.Fatalf("ConvertLSIFToSCIP: %v", err)
	}
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.GetMetadata().GetTextDocumentEncoding() != scip.TextEncoding_UTF16 {
		t.Fatalf("utf-16 stream must keep the UTF-16 declaration, got %s",
			index.GetMetadata().GetTextDocumentEncoding())
	}
}

func TestConvertLSIFToSCIPRejectsUndeclaredEncoding(t *testing.T) {
	_, stream := lsifEncodingFixture(t, "", "héllo world\n", 6, 7)
	if _, err := ConvertLSIFToSCIP(stream); err == nil {
		t.Fatal("a stream without a positionEncoding declaration must be rejected by the SCIP conversion")
	}
}
