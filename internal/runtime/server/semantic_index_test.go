package server

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
)

type semanticRegistrationBackend struct{ *mockBackend }

func (semanticRegistrationBackend) ExportIndex(context.Context, model.Request, model.Sink) (model.Report, error) {
	return model.Report{}, nil
}
func (semanticRegistrationBackend) BuildIndexRequest(context.Context, model.WorkspaceView, string) (model.Request, error) {
	return model.Request{}, nil
}

var _ languages.Backend = semanticRegistrationBackend{}
var _ languages.SemanticIndexProvider = semanticRegistrationBackend{}
var _ languages.SemanticIndexRequestBuilder = semanticRegistrationBackend{}

func TestRegisterBackendBindsSemanticProviderAndPlanner(t *testing.T) {
	s := New(DefaultConfig())
	backend := semanticRegistrationBackend{mockBackend: &mockBackend{langID: "go"}}
	s.RegisterBackend("go", backend)
	s.RegisterBackend("go-alias", backend)
	bindings := s.semanticIndexBindings()
	if len(bindings) != 1 || bindings[0].language != "go" || bindings[0].binding.provider == nil || bindings[0].binding.planner == nil {
		t.Fatalf("semantic bindings = %#v", bindings)
	}
}

func TestRegisterBackendAliasKeepsCanonicalSemanticLanguage(t *testing.T) {
	s := New(DefaultConfig())
	backend := semanticRegistrationBackend{mockBackend: &mockBackend{langID: "cpp"}}
	s.RegisterBackend("cpp", backend)
	s.RegisterBackend("c", backend)
	bindings := s.semanticIndexBindings()
	if len(bindings) != 1 || bindings[0].language != "cpp" {
		t.Fatalf("alias created a non-canonical semantic binding: %#v", bindings)
	}
}

func TestRegisterExternalSemanticIndexProvider(t *testing.T) {
	s := New(DefaultConfig())
	provider := semanticRegistrationBackend{}
	s.RegisterSemanticIndexProvider("python", provider, provider)
	bindings := s.semanticIndexBindings()
	if len(bindings) != 1 || bindings[0].language != "python" {
		t.Fatalf("semantic bindings = %#v", bindings)
	}
}
