package interop

import (
	"sort"
	"strings"
	"testing"
)

// sampleIndex 构造带身份信息与多角色出现点的规范索引。
func sampleIndex() Index {
	idx := FromOccurrences("internal/foo/a.go", []RawOccurrence{
		{Name: "go github.com/omnilsp/omni Foo.", StartLine: 0, StartChar: 5, EndLine: 0, EndChar: 8, Definition: true},
		{Name: "go github.com/omnilsp/omni Foo.", StartLine: 9, StartChar: 2, EndLine: 9, EndChar: 5},
		{Name: "go github.com/omnilsp/omni Bar", StartLine: 3, StartChar: 0, EndLine: 3, EndChar: 3, Definition: true},
	})
	idx.Documents[0].LanguageID = "go"
	for i := range idx.Symbols {
		if strings.HasSuffix(idx.Symbols[i].Name, "Foo.") {
			idx.Symbols[i].Kind = KindClass
			idx.Symbols[i].Signature = "type Foo struct{}"
		}
	}
	idx.Metadata = Metadata{
		RepoIdentity: "github.com/omnilsp/omni",
		CommitID:     "abc1234def",
		ToolVersion:  "test-1.0.0",
	}
	return idx
}

type occKey struct {
	path string
	name string
	rng  Range
	role Role
}

func occSet(idx Index) map[occKey]int {
	m := map[occKey]int{}
	for _, d := range idx.Documents {
		for _, o := range d.Occurrences {
			k := occKey{o.DocPath, o.SymbolName, o.Range, o.Role}
			m[k]++
		}
	}
	return m
}

func symNames(idx Index) []string {
	var out []string
	for _, s := range idx.Symbols {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func TestL14_SCIPRoundTrip(t *testing.T) {
	in := sampleIndex()
	wire, err := ExportSCIP(in)
	if err != nil {
		t.Fatalf("ExportSCIP: %v", err)
	}
	if len(wire.GetDocuments()) != len(in.Documents) {
		t.Fatalf("documents 数不匹配: got %d want %d", len(wire.GetDocuments()), len(in.Documents))
	}
	out, err := ImportSCIP(wire)
	if err != nil {
		t.Fatalf("ImportSCIP: %v", err)
	}
	if len(out.Documents) != len(in.Documents) {
		t.Errorf("round-trip documents 数: got %d want %d", len(out.Documents), len(in.Documents))
	}
	if got, want := strings.Join(symNames(out), ","), strings.Join(symNames(in), ","); got != want {
		t.Errorf("symbol 名集合: got %q want %q", got, want)
	}
	got, want := occSet(out), occSet(in)
	if len(got) != len(want) {
		t.Fatalf("occurrence 集合大小: got %d want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("occurrence %+v 计数: got %d want %d", k, got[k], n)
		}
	}
}

func TestL15_LSIFRoundTrip(t *testing.T) {
	in := sampleIndex()
	data, err := ExportLSIF(in)
	if err != nil {
		t.Fatalf("ExportLSIF: %v", err)
	}
	out, err := ImportLSIF(data)
	if err != nil {
		t.Fatalf("ImportLSIF: %v", err)
	}
	if len(out.Documents) != len(in.Documents) {
		t.Errorf("round-trip documents 数: got %d want %d", len(out.Documents), len(in.Documents))
	}
	if got, want := strings.Join(symNames(out), ","), strings.Join(symNames(in), ","); got != want {
		t.Errorf("symbol 名集合: got %q want %q", got, want)
	}
	got, want := occSet(out), occSet(in)
	if len(got) != len(want) {
		t.Fatalf("occurrence 集合大小: got %d want %d\n导出样例:\n%.400s", len(got), len(want), data)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("occurrence %+v 计数: got %d want %d", k, got[k], n)
		}
	}
}

func TestL14_LossyNotesDocumented(t *testing.T) {
	notes := LossyNotes()
	if len(notes) == 0 {
		t.Fatal("LossyNotes() 为空：§L14 要求有损映射必须文档化")
	}
	for i, n := range notes {
		if !strings.Contains(n, "有损") {
			t.Errorf("LossyNotes()[%d] 缺少「有损」关键词: %q", i, n)
		}
		if len(n) < 20 {
			t.Errorf("LossyNotes()[%d] 过短，不像有效文档: %q", i, n)
		}
	}
}

func TestL11_IdentityPreserved(t *testing.T) {
	in := sampleIndex()

	scipIdx, err := ExportSCIP(in)
	if err != nil {
		t.Fatalf("ExportSCIP: %v", err)
	}
	scipOut, err := ImportSCIP(scipIdx)
	if err != nil {
		t.Fatalf("ImportSCIP: %v", err)
	}
	if scipOut.Metadata.RepoIdentity != in.Metadata.RepoIdentity {
		t.Errorf("SCIP RepoIdentity 丢失: got %q want %q", scipOut.Metadata.RepoIdentity, in.Metadata.RepoIdentity)
	}
	if scipOut.Metadata.CommitID != in.Metadata.CommitID {
		t.Errorf("SCIP CommitID 丢失: got %q want %q", scipOut.Metadata.CommitID, in.Metadata.CommitID)
	}

	lsifData, err := ExportLSIF(in)
	if err != nil {
		t.Fatalf("ExportLSIF: %v", err)
	}
	lsifOut, err := ImportLSIF(lsifData)
	if err != nil {
		t.Fatalf("ImportLSIF: %v", err)
	}
	if lsifOut.Metadata.RepoIdentity != in.Metadata.RepoIdentity {
		t.Errorf("LSIF RepoIdentity 丢失: got %q want %q", lsifOut.Metadata.RepoIdentity, in.Metadata.RepoIdentity)
	}
	if lsifOut.Metadata.CommitID != in.Metadata.CommitID {
		t.Errorf("LSIF CommitID 丢失: got %q want %q", lsifOut.Metadata.CommitID, in.Metadata.CommitID)
	}
}

func TestImportProvenanceMarked(t *testing.T) {
	in := sampleIndex()

	scipWire, err := ExportSCIP(in)
	if err != nil {
		t.Fatalf("ExportSCIP: %v", err)
	}
	scipOut, err := ImportSCIP(scipWire)
	if err != nil {
		t.Fatalf("ImportSCIP: %v", err)
	}
	if scipOut.Metadata.Provenance == "" {
		t.Error("SCIP 导入产物 Provenance 为空（§L14 违规）")
	}
	if scipOut.Metadata.FreshnessLimit == "" {
		t.Error("SCIP 导入产物 FreshnessLimit 为空（§L14 违规）")
	}

	lsifData, err := ExportLSIF(in)
	if err != nil {
		t.Fatalf("ExportLSIF: %v", err)
	}
	lsifOut, err := ImportLSIF(lsifData)
	if err != nil {
		t.Fatalf("ImportLSIF: %v", err)
	}
	if lsifOut.Metadata.Provenance != lsifProvenance {
		t.Errorf("LSIF 导入产物 Provenance = %q，want %q", lsifOut.Metadata.Provenance, lsifProvenance)
	}
	if lsifOut.Metadata.FreshnessLimit == "" {
		t.Error("LSIF 导入产物 FreshnessLimit 为空（§L14 违规）")
	}
}
