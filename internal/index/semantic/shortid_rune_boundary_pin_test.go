package semantic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

// Pin for verifierB-r23b entry 4: shortID truncated a symbol ID at byte 77
// and appended "...", splitting multi-byte runes — validIdentifier accepts
// any valid UTF-8, so a 97-byte multi-byte symbol ID came back from shortID
// with utf8.ValidString == false, and the escaped split byte reached the
// user-visible ErrUnknownSymbol / ErrDuplicateSymbol messages as literal
// \xe9 noise. Truncation must land on a rune boundary.
func TestShortIDTruncatesAtRuneBoundary(t *testing.T) {
	id := "go:pkg." + strings.Repeat("雪", 30) // 7 + 30×3 = 97 bytes
	if utf8.RuneCountInString(id) <= 26 {
		t.Fatal("fixture precondition: the ID must be long enough to truncate")
	}
	got := shortID(id)
	if !utf8.ValidString(got) {
		t.Fatalf("shortID split a multi-byte rune: %q (len %d)", got, len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("shortID must still mark the abbreviation: %q", got)
	}
	if len(got) > 80 {
		t.Fatalf("shortID must still bound the display length, got %d bytes", len(got))
	}
	// An ASCII ID keeps the exact 80-byte shape (77 content bytes + "...").
	ascii := shortID(strings.Repeat("a", 120))
	if ascii != strings.Repeat("a", 77)+"..." {
		t.Fatalf("ASCII truncation changed shape: %q", ascii)
	}
}

// End-to-end: referencing an undeclared multi-byte symbol surfaces the
// abbreviated ID through Finalize's ErrUnknownSymbol — the user-visible
// message must not carry the escaped fragment of a split rune.
func TestFinalizeUnknownSymbolMessageKeepsRuneBoundaries(t *testing.T) {
	ctx := context.Background()
	store, _, req, report, sourceHash := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sink.Close()
		_ = build.Abort()
	}()
	scope := req.Scopes[0]
	symbol := identity.SymbolID("go:pkg." + strings.Repeat("雪", 30))
	occ := model.Occurrence{
		SymbolID: symbol, ScopeID: scope.ID,
		URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 3},
		Role: "reference", SourceHash: sourceHash, BuildContext: scope.BuildContext,
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{occ}); err != nil {
		t.Fatal(err)
	}
	err = sink.Finalize(ctx, report)
	if !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("Finalize error = %v, want ErrUnknownSymbol", err)
	}
	msg := err.Error()
	if !utf8.ValidString(msg) {
		t.Fatalf("error message is not valid UTF-8: %q", msg)
	}
	if strings.Contains(msg, "\\xe9") {
		t.Fatalf("a split rune escaped into the user-visible message: %q", msg)
	}
	if !strings.Contains(msg, "go:pkg.") {
		t.Fatalf("message lost the abbreviated symbol identity: %q", msg)
	}
}
