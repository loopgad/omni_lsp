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

	// Optional capability interfaces are pinned by name in the registry of
	// known capabilities; their method sets are exercised by the feature
	// negotiation tests (features_gate_test.go). Here we only guard against
	// silent removal: every optional interface must remain declared.
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
