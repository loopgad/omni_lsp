package main

// Registration seam guards for the explicit cpp/c and js-family semantic
// index registration (behavior-authorization round). registerBackends binds
// the ccls exporter capability to both language keys the bridge serves, and
// the pinned TypeScript capability to every TS/JS dialect key its semantic
// index handles. These tests pin the registration product: the explicit key
// set on a real ccls backend and the exact TS/JS dialect key list.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// TestRegisterCclsSemanticIndexBindsCanonicalAndAliasKeys drives the real
// registerBackends registration product for the ccls bridge: the capability
// must be explicitly registered under the canonical "cpp" key and the "c"
// alias, so the dirty workspace/symbol guard flips for cpp and the alias keys
// resolve the shared capability.
func TestRegisterCclsSemanticIndexBindsCanonicalAndAliasKeys(t *testing.T) {
	cb, err := ccls.New(t.TempDir())
	if err != nil {
		t.Skipf("clangd unavailable in this environment: %v", err)
	}
	defer func() { _ = cb.Close() }()

	srv := server.New(server.DefaultConfig())
	srv.RegisterBackend(cb.LanguageID(), cb)
	srv.RegisterBackend("c", cb)
	registerCclsSemanticIndex(srv, cb)

	keys := explicitSemanticIndexKeys(t, srv)
	want := []string{"c", "cpp"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("explicit semantic index keys = %v, want %v", keys, want)
	}
}

// TestTypescriptSemanticIndexLanguagesCoverJSFamily pins the dialect key list
// the nested registration loop binds to the pinned TypeScript capability.
func TestTypescriptSemanticIndexLanguagesCoverJSFamily(t *testing.T) {
	want := []string{"typescript", "javascript", "typescriptreact", "javascriptreact"}
	if !reflect.DeepEqual(typescriptSemanticIndexLanguages, want) {
		t.Fatalf("typescript semantic index languages = %v, want %v", typescriptSemanticIndexLanguages, want)
	}
}

// explicitSemanticIndexKeys reads the server's explicit-registration set for
// assertions from outside the server package (read-only reflection).
func explicitSemanticIndexKeys(t *testing.T, srv *server.Server) []string {
	t.Helper()
	value := reflect.ValueOf(srv).Elem().FieldByName("explicitSemanticIndexes")
	if !value.IsValid() || value.Kind() != reflect.Map {
		t.Fatal("server.explicitSemanticIndexes is missing or not a map")
	}
	keys := make([]string, 0, value.Len())
	for _, key := range value.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	return keys
}
