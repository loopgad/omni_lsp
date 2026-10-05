package languages_test

import (
	"reflect"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/languages/pyright"
	"github.com/omnilsp/omni/internal/languages/rustanalyzer"
	"github.com/omnilsp/omni/internal/languages/typescript"
)

// realBackends is every backend that ships, keyed by package name. Values are
// the pointer type, taken from a nil pointer so nothing is constructed: ccls
// and the nested bridges spawn processes in New, and this test needs no
// toolchain, no workspace and no process to run in milliseconds.
var realBackends = map[string]reflect.Type{
	"ccls":         reflect.TypeOf((*ccls.Backend)(nil)),
	"golang":       reflect.TypeOf((*golang.Backend)(nil)),
	"pyright":      reflect.TypeOf((*pyright.Backend)(nil)),
	"rustanalyzer": reflect.TypeOf((*rustanalyzer.Backend)(nil)),
	"typescript":   reflect.TypeOf((*typescript.Backend)(nil)),
}

// optionalCapabilities is every optional interface in backend.go that production
// code discovers by runtime type assertion. The core Backend interface is not
// listed: srv.RegisterBackend takes a languages.Backend, so a missing method is
// a compile error. These have no such net.
var optionalCapabilities = map[string]any{
	"CompletionListProvider":        (*languages.CompletionListProvider)(nil),
	"EncodedDiagnosticsProvider":    (*languages.EncodedDiagnosticsProvider)(nil),
	"WorkspaceSnapshotSynchronizer": (*languages.WorkspaceSnapshotSynchronizer)(nil),
	"EncodedSemanticTokensProvider": (*languages.EncodedSemanticTokensProvider)(nil),
	"SignatureHelper":               (*languages.SignatureHelper)(nil),
	"Formatter":                     (*languages.Formatter)(nil),
	"InlayHintProvider":             (*languages.InlayHintProvider)(nil),
	"StatusReporter":                (*languages.StatusReporter)(nil),
	"IncompleteCompletionProvider":  (*languages.IncompleteCompletionProvider)(nil),
	"DeclarationProvider":           (*languages.DeclarationProvider)(nil),
}

// TestOptionalCapabilitiesStillReachableBySomeBackend freezes the method sets
// of the optional capability interfaces.
//
// backend_shape_test.go pins the core 12 methods and asserts only that these
// interfaces still exist. That leaves the failure mode ADR-0009 exists to
// prevent: capabilities are wired with backend.(languages.DeclarationProvider),
// so deleting a method from the interface compiles everywhere, every assertion
// silently starts returning false, and the feature disappears from the server
// with no test failing and no build break. Requiring at least one real backend
// to still satisfy each interface turns that silence into a failure.
//
// The method names are deliberately not transcribed. Copying them here would
// be a second source of truth that goes stale, and it would fail on an
// interface gaining a method -- which is the normal way a capability grows.
// What must not happen is an interface becoming unreachable.
func TestOptionalCapabilitiesStillReachableBySomeBackend(t *testing.T) {
	for name, iface := range optionalCapabilities {
		ifaceType := reflect.TypeOf(iface).Elem()
		if ifaceType.Kind() != reflect.Interface {
			t.Errorf("%s is not an interface", name)
			continue
		}
		if ifaceType.NumMethod() == 0 {
			t.Errorf("%s declares no methods; drop it instead of asserting on it", name)
			continue
		}
		var implementers []string
		for pkg, backendType := range realBackends {
			if backendType.Implements(ifaceType) {
				implementers = append(implementers, pkg)
			}
		}
		if len(implementers) == 0 {
			t.Errorf("no shipped backend satisfies %s, so every %s assertion in "+
				"production returns false and the capability is dead code. "+
				"Its methods: %v", name, name, methodNames(ifaceType))
		}
	}
}

func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	return out
}
