package languages

import (
	"reflect"
	"testing"
)

// TestV4_CoreInterfaceShapeFreeze pins the §V4 core-interface freeze: the 12
// canonical Backend methods must never be added to, removed, or reordered.
// New capabilities enter as SEPARATE optional interfaces (SignatureHelper et
// al.), each of which is also pinned here. A breaking change to this shape
// requires an ADR and a spec amendment (§U10) — this test is the tripwire.
func TestV4_CoreInterfaceShapeFreeze(t *testing.T) {
	backendType := reflect.TypeOf((*Backend)(nil)).Elem()
	var got []string
	for i := 0; i < backendType.NumMethod(); i++ {
		got = append(got, backendType.Method(i).Name)
	}
	want := []string{
		"Close",
		"Completion",
		"Definition",
		"Diagnostics",
		"DocumentSymbols",
		"FileExtensions",
		"Hover",
		"LanguageID",
		"References",
		"Rename",
		"SemanticTokens",
		"WorkspaceSymbols",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("core Backend interface drifted:\n got %v\nwant %v", got, want)
	}

	// Optional capability interfaces are declared here so none is deleted
	// outright. Their method sets are the part that actually matters, because
	// production finds them by type assertion rather than by parameter type, and
	// backend_conformance_test.go is what pins that: it requires at least one
	// shipped backend to still satisfy each one.
	for _, iface := range []any{
		(*SignatureHelper)(nil),
		(*Formatter)(nil),
		(*InlayHintProvider)(nil),
		(*StatusReporter)(nil),
		(*IncompleteCompletionProvider)(nil),
	} {
		if reflect.TypeOf(iface).Elem().Kind() != reflect.Interface {
			t.Fatalf("%T is not an interface", iface)
		}
	}
}
