package golang

// Tests for goal.md §D7/INV-POS-001: LSP UTF-16 positions must map to the
// exact logical boundary in Go source containing multi-byte characters.

import (
	"context"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

const cjkSource = `package main

// 中文注释：问候语
func greet(名字 string) string {
	return "你好, " + 名字
}
`

func TestINV_POS_001_HoverOnCJKIdentifier(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", cjkSource)

	// Locate 名字 on the return line (0-based); compute its UTF-16 column by
	// counting runes of the prefix (CJK prefix bytes != columns).
	lines := strings.Split(cjkSource, "\n")
	var wantLine, col uint32
	for i, l := range lines {
		if idx := strings.Index(l, "名字"); idx >= 0 && strings.HasPrefix(l, "\treturn") {
			wantLine = uint32(i)
			col = uint32(len([]rune(l[:idx])))
			break
		}
	}
	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: uri, Content: []byte(cjkSource),
		Line: wantLine, Column: col,
	})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if res.Value == nil || !strings.Contains(res.Value.Contents, "名字") {
		t.Fatalf("hover at %d:%d = %+v, want 名字 signature", wantLine, col, res.Value)
	}
}

func TestINV_POS_001_DefinitionRoundTripThroughUTF16(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", cjkSource)

	lines := strings.Split(cjkSource, "\n")
	var useLine, useCol uint32
	for i, l := range lines {
		if idx := strings.Index(l, "+ 名字"); idx >= 0 {
			useLine = uint32(i)
			useCol = uint32(len([]rune(l[:strings.Index(l, "名字")])))
			break
		}
	}
	locs, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: uri, Content: []byte(cjkSource),
		Line: useLine, Column: useCol,
	})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs.Value) == 0 {
		t.Fatal("no definition found")
	}
	def := locs.Value[0]
	// The definition is the parameter declaration on line 3; its column must
	// be a UTF-16 column, i.e. point at 名 and not into the middle of it.
	defLine := def.Range.StartLine
	defCol := def.Range.StartCharacter
	src := strings.Split(cjkSource, "\n")[defLine]
	runes := []rune(src)
	if int(defCol) >= len(runes) || runes[defCol] != '名' {
		t.Errorf("definition position %d:%d does not land on 名 (landed on %q)",
			defLine, defCol, safeRuneAt(src, int(defCol)))
	}
}

func TestINV_POS_001_ReferencesExcludeMidRuneOffsets(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", cjkSource)

	lines := strings.Split(cjkSource, "\n")
	var declLine, declCol uint32
	for i, l := range lines {
		if idx := strings.Index(l, "名字 string"); idx >= 0 {
			declLine = uint32(i)
			declCol = uint32(len([]rune(l[:idx])))
			break
		}
	}
	refs, err := b.References(context.Background(), languages.ReferencesRequest{
		URI: uri, Content: []byte(cjkSource),
		Line: declLine, Column: declCol, IncludeDecl: true,
	})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs.Value) < 2 {
		t.Fatalf("expected declaration + usage refs, got %d", len(refs.Value))
	}
	for _, r := range refs.Value {
		lineText := strings.Split(cjkSource, "\n")[r.Range.StartLine]
		start := []rune(lineText)
		if int(r.Range.StartCharacter) >= len(start) {
			t.Errorf("ref column out of range: %d:%d", r.Range.StartLine, r.Range.StartCharacter)
			continue
		}
		if start[r.Range.StartCharacter] != '名' {
			t.Errorf("ref at %d:%d lands on %q, want 名",
				r.Range.StartLine, r.Range.StartCharacter, start[r.Range.StartCharacter])
		}
	}
}

func safeRuneAt(s string, i int) string {
	r := []rune(s)
	if i < len(r) {
		return string(r[i])
	}
	return "<eof>"
}

func TestE0_BackendBuildContextID(t *testing.T) {
	b := New(t.TempDir())
	defer b.Close()
	id1 := b.BuildContextID()
	id2 := b.BuildContextID()
	if id1 == "" {
		t.Fatal("empty BuildContextID")
	}
	if id1 != id2 {
		t.Errorf("BuildContextID unstable: %s vs %s", id1, id2)
	}
	// Either a real digest or the explicit unavailable marker — never a guess.
	if !strings.Contains(string(id1), "go:sha256:") {
		t.Errorf("ID format wrong: %q", id1)
	}
}
