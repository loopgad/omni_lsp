package typescript

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

func TestPinnedSemanticIndexProviderCapturesExecutableIdentities(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("Node runtime is not installed: %v", err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve runtime provider test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	typescriptModule := filepath.Join(repoRoot, "test", "acceptance", "tools", "node_modules", "typescript", "lib", "typescript.js")
	if _, err := os.Stat(typescriptModule); err != nil {
		t.Skipf("pinned TypeScript test tool is not installed: %v", err)
	}
	t.Setenv(semanticNodePathEnv, node)
	t.Setenv(semanticTSPathEnv, typescriptModule)

	provider, err := NewPinnedSemanticIndexProvider(context.Background(), &Backend{})
	if err != nil {
		if version, probeErr := probeToolVersion(context.Background(), node, "--version"); probeErr == nil && version != semanticIndexNodeVersion {
			t.Skipf("local Node %s does not match pinned %s", version, semanticIndexNodeVersion)
		}
		t.Fatalf("NewPinnedSemanticIndexProvider: %v", err)
	}
	if provider.config.Runner == nil || len(provider.config.Tools) != 3 {
		t.Fatalf("provider config runner/tools = %T/%d, want runner and three identities", provider.config.Runner, len(provider.config.Tools))
	}
	if err := verifyPinnedToolFiles(context.Background(), provider.config.Tools); err != nil {
		t.Fatalf("verify captured tool identities: %v", err)
	}
	if err := verifyEmbeddedExporter(context.Background(), provider.config.Tools); err != nil {
		t.Fatalf("verify embedded adapter identity: %v", err)
	}
	backend, epoch := provider.config.BackendState("javascript")
	if backend != (identity.BackendID{Language: "javascript", Name: serverName}) || epoch != 0 {
		t.Fatalf("backend state = %+v epoch %d", backend, epoch)
	}
}

func TestSemanticIndexRequestUsesCurrentBackendEpochPerProjectLanguage(t *testing.T) {
	view := &semanticTestView{
		id:    model.Identity{Workspace: "workspace", DiskDigest: "sha256:disk", SnapshotRev: 17},
		files: map[string][]model.File{},
	}
	var called []string
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		Tools: semanticTools(t),
		BuildScopes: func(context.Context, model.WorkspaceView, string) ([]model.Scope, error) {
			return []model.Scope{
				{ID: "ts", Language: "typescript", RootURI: "file:///repo/ts"},
				{ID: "js", Language: "javascript", RootURI: "file:///repo/js"},
			}, nil
		},
		BackendState: func(language string) (identity.BackendID, identity.BackendEpoch) {
			called = append(called, language)
			return identity.BackendID{Language: language, Name: "tsserver"}, 12
		},
	})

	request, err := provider.BuildIndexRequest(context.Background(), view, "file:///repo")
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(called) != 2 || called[0] != "typescript" || called[1] != "javascript" {
		t.Fatalf("backend state languages = %v, want TypeScript and JavaScript", called)
	}
	for _, scope := range request.Scopes {
		provenance := request.Provenance[scope.ID]
		if provenance.Backend.Language != scope.Language || provenance.Backend.Name != "tsserver" || provenance.BackendEpoch != 12 {
			t.Errorf("scope %s provenance backend = %+v epoch %d", scope.ID, provenance.Backend, provenance.BackendEpoch)
		}
	}
}
