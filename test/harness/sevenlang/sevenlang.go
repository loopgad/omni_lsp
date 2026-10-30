// Package sevenlang builds real per-language typed-fact export requests for
// the seven registered languages (go, rust, typescript, python, cpp, c,
// javascript). It is a test-support package for the DEF-IDXWIRE evidence
// lanes: each builder drives the language's own production export surface
// (SemanticIndexProvider/Backend BuildIndexRequest + ExportIndex) against a
// small on-disk fixture and the pinned host toolchain, so the persistent-chain
// and cross-process replay tests exercise the same typed fact stream the
// server binds at registration time.
//
// Toolchain gaps surface as errors wrapping ErrUnavailable so callers can skip
// with the exact blocking point instead of failing the lane.
package sevenlang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/languages/pyright"
	"github.com/omnilsp/omni/internal/languages/rustanalyzer"
	"github.com/omnilsp/omni/internal/languages/typescript"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// ErrUnavailable marks a language whose toolchain is not usable on this host.
// Callers must skip the language and surface the reason verbatim.
var ErrUnavailable = errors.New("sevenlang: language toolchain unavailable")

func unavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
}

// Languages lists the seven registered language keys in pinned order.
func Languages() []string {
	return []string{"go", "rust", "typescript", "python", "cpp", "c", "javascript"}
}

// GoEnvPins are the environment pins the Go exporter chain requires. They
// mirror internal/languages/golang's own semantic-index tests so the evidence
// lane runs the same deterministic tool path (no proxy, no toolchain
// switching, no workspace interference).
func GoEnvPins() map[string]string {
	return map[string]string{
		"GOFLAGS": "", "GOWORK": "auto", "GOPROXY": "off", "GOSUMDB": "off",
		"GOENV": "off", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off",
	}
}

// Expectation states the typed facts a committed language generation must
// serve back after a fresh-only re-read. Checks are minimums so per-language
// extractors may honestly report more.
type Expectation struct {
	SymbolNames    []string                              // display names that must appear among the scope's symbols
	ExtraSymbol    string                                // name present only in the extra-symbol variant
	MinOccurrences int                                   // minimum occurrence facts for the scope
	MinDefinitions int                                   // minimum definition-role occurrences
	EdgeKinds      map[model.EdgeKind]int                // minimum typed edges per kind
	Coverage       map[model.FactKind]model.Completeness // exact coverage states that must hold
}

// Bundle is one language's ready-to-commit export: the validated request, the
// production export entry point, and the expectation it must satisfy.
type Bundle struct {
	Language string
	View     model.WorkspaceView
	Request  model.Request
	Export   func(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error)
	Close    func() error // release backend resources; may be nil
	Expect   Expectation
	Notes    []string // honest caveats about the language's chain (e.g. alias keys)
}

// Build prepares one language's export bundle in dir. extra adds a second
// variant symbol so callers can prove fresh-only reads across a recommit.
func Build(ctx context.Context, language, dir string, extra bool) (Bundle, error) {
	switch language {
	case "go":
		return buildGo(ctx, dir, extra)
	case "rust":
		return buildRust(ctx, dir, extra)
	case "typescript":
		return buildTypeScript(ctx, dir, extra, "typescript")
	case "javascript":
		return buildTypeScript(ctx, dir, extra, "javascript")
	case "python":
		return buildPython(ctx, dir, extra)
	case "cpp":
		return buildClang(ctx, dir, extra, "cpp")
	case "c":
		return buildClang(ctx, dir, extra, "c")
	default:
		return Bundle{}, fmt.Errorf("sevenlang: unknown language %q", language)
	}
}

// ---------------------------------------------------------------------------
// shared immutable view
// ---------------------------------------------------------------------------

// view is an immutable model.WorkspaceView + WorkspaceMaterializer over a
// pre-materialized on-disk snapshot. Materialize returns the pre-written
// snapshot directory (never the caller's destination), the same borrowed-root
// pattern internal/languages/ccls and rustanalyzer fixtures use: extractors
// verify every file by digest, and rust's isolation check requires the
// borrowed root to sit outside its own scratch tree.
type view struct {
	id       model.Identity
	rootURI  string
	rootPath string
	files    []model.File
	content  map[string][]byte
}

type materializedView struct {
	rootURI  string
	rootPath string
	owner    *view
}

func (m materializedView) RootURI() string  { return m.rootURI }
func (m materializedView) RootPath() string { return m.rootPath }
func (m materializedView) Close() error     { return nil }

func (m materializedView) PathForURI(uri string) (string, error) {
	parsed, err := workspaceuri.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("map URI %q into the captured snapshot: %w", uri, err)
	}
	path, err := parsed.Path()
	if err != nil {
		return "", fmt.Errorf("map URI %q into the captured snapshot: %w", uri, err)
	}
	root, err := workspaceuri.Parse(m.rootURI)
	if err != nil {
		return "", err
	}
	rootPath, err := root.Path()
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("URI %q is outside the captured root %q", uri, m.rootURI)
	}
	return filepath.Join(m.rootPath, filepath.FromSlash(relative)), nil
}

func (v *view) Identity() model.Identity { return v.id }

func (v *view) Walk(ctx context.Context, root string, visit func(model.File) error) error {
	prefix := strings.TrimSuffix(root, "/") + "/"
	if strings.TrimSuffix(v.rootURI, "/") != strings.TrimSuffix(root, "/") {
		// Scopes may walk a subtree; keep only files under it.
		prefix = root
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
	}
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(file.URI, prefix) {
			continue
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *view) Read(_ context.Context, uri string) (io.ReadCloser, error) {
	if data, ok := v.content[uri]; ok {
		return io.NopCloser(strings.NewReader(string(data))), nil
	}
	return nil, fmt.Errorf("sevenlang view: no file %q", uri)
}

func (v *view) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSuffix(rootURI, "/") != strings.TrimSuffix(v.rootURI, "/") {
		return nil, fmt.Errorf("materialize root %q does not match the captured view %q", rootURI, v.rootURI)
	}
	destinationAbs, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	rootAbs, err := filepath.Abs(v.rootPath)
	if err != nil {
		return nil, err
	}
	if pathWithin(destinationAbs, rootAbs) || pathWithin(rootAbs, destinationAbs) {
		return nil, fmt.Errorf("materialize destination %q overlaps the captured snapshot %q", destination, v.rootPath)
	}
	return materializedView{rootURI: v.rootURI, rootPath: v.rootPath, owner: v}, nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != ".." && !filepath.IsAbs(relative))
}

func languageIDFor(relative string) string {
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts":
		return "typescript"
	case ".js":
		return "javascript"
	case ".py":
		return "python"
	case ".c", ".h":
		return "c"
	default:
		return "cpp"
	}
}

// newView writes files (slash-separated relative paths) into dir and captures
// the immutable manifest with real content digests.
func newView(dir, workspace string, files map[string][]byte) (*view, error) {
	rootPath, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	rootURI := workspaceuri.FromPath(rootPath).Canonical()
	v := &view{
		rootURI:  rootURI,
		rootPath: rootPath,
		content:  make(map[string][]byte, len(files)),
		files:    make([]model.File, 0, len(files)),
	}
	relativePaths := make([]string, 0, len(files))
	for relative := range files {
		relativePaths = append(relativePaths, relative)
	}
	sort.Strings(relativePaths)
	digest := sha256.New()
	for _, relative := range relativePaths {
		body := append([]byte(nil), files[relative]...)
		local := filepath.Join(rootPath, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(local, body, 0o600); err != nil {
			return nil, err
		}
		fileURI := workspaceuri.FromPath(local).Canonical()
		sum := sha256.Sum256(body)
		v.files = append(v.files, model.File{
			URI:        fileURI,
			LanguageID: languageIDFor(relative),
			Size:       int64(len(body)),
			SHA256:     identity.ContentHash("sha256:" + hex.EncodeToString(sum[:])),
		})
		v.content[fileURI] = body
		_, _ = fmt.Fprintf(digest, "%d:%s", len(fileURI), fileURI)
		_, _ = fmt.Fprintf(digest, "%d:", len(body))
		_, _ = digest.Write(body)
	}
	v.id = model.Identity{
		Workspace:   identity.WorkspaceID("sevenlang/" + workspace),
		DiskDigest:  identity.ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil))),
		SnapshotRev: 1,
	}
	return v, nil
}

func (v *view) workspaceIdentity() model.Identity { return v.id }

// toolFromPath pins a real executable identity (path, version, content digest).
func toolFromPath(name, path, version string) (model.ToolIdentity, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	digest, err := fileSHA256(absolute)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	return model.ToolIdentity{Name: name, Path: absolute, Version: version, SHA256: digest}, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func probeVersion(path string) (string, error) {
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("probe %s: %w (%s)", path, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// hostExecutableTool pins the running test binary as the embedded-exporter
// identity, exactly like the per-language direct-runner fixtures.
func hostExecutableTool(name, version string) (model.ToolIdentity, error) {
	self, err := os.Executable()
	if err != nil {
		return model.ToolIdentity{}, err
	}
	return toolFromPath(name, self, version)
}

func repoRoot() (string, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("sevenlang: cannot locate repository root from source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..")), nil
}

func requireFile(path, missing string) error {
	if _, err := os.Stat(path); err != nil {
		return unavailable("%s is not installed: %v", missing, err)
	}
	return nil
}

// pinnedPyrightRuntime pins Node to the exact version the audited Pyright
// bundle requires (internal/languages/pyright semanticIndexRuntimeVersion).
const pyrightPinnedNodeVersion = "24.14.0"

const pyrightCompilerVersion = "1.1.414"

const pyrightExtractorVersion = "3"

// ---------------------------------------------------------------------------
// go
// ---------------------------------------------------------------------------

func buildGo(ctx context.Context, dir string, extra bool) (Bundle, error) {
	libValue := "package lib\n\n// Value is the library anchor the app module calls into.\nfunc Value() int { return 7 }\n"
	if extra {
		libValue += "\n// Spare is the recommit marker symbol.\nfunc Spare() int { return 9 }\n"
	}
	files := map[string][]byte{
		"go.work":      []byte("go 1.26.1\n\nuse (\n\t./app\n\t./lib\n)\n"),
		"app/go.mod":   []byte("module example.com/app\n\ngo 1.26.1\n\nrequire example.com/lib v0.0.0\n"),
		"lib/go.mod":   []byte("module example.com/lib\n\ngo 1.26.1\n"),
		"lib/value.go": []byte(libValue),
		"app/use.go":   []byte("package app\n\nimport \"example.com/lib\"\n\n// Use calls into the library module across the workspace boundary.\nfunc Use() int { return lib.Value() }\n"),
	}
	v, err := newView(dir, "go", files)
	if err != nil {
		return Bundle{}, err
	}
	backend := golang.New(v.rootPath)
	request, err := backend.BuildIndexRequest(ctx, v, v.rootURI)
	if err != nil {
		_ = backend.Close()
		return Bundle{}, err
	}
	expectation := Expectation{
		SymbolNames:    []string{"Use", "Value"},
		ExtraSymbol:    "Spare",
		MinOccurrences: 2,
		MinDefinitions: 2,
		EdgeKinds: map[model.EdgeKind]int{
			model.EdgeCall:   1,
			model.EdgeImport: 1,
			model.EdgeModule: 1,
		},
		Coverage: map[model.FactKind]model.Completeness{
			model.FactInclude: model.Unavailable,
		},
	}
	return Bundle{
		Language: "go",
		View:     v,
		Request:  request,
		Export:   backend.ExportIndex,
		Close:    backend.Close,
		Expect:   expectation,
	}, nil
}

// ---------------------------------------------------------------------------
// rust
// ---------------------------------------------------------------------------

func buildRust(ctx context.Context, dir string, extra bool) (Bundle, error) {
	tools := make([]model.ToolIdentity, 0, 3)
	rustAnalyzerVersion := ""
	rustcVersion := ""
	for _, pair := range []struct{ command, name string }{
		{"rust-analyzer", "rust-analyzer"}, {"cargo", "cargo"}, {"rustc", "rustc"},
	} {
		path, err := exec.LookPath(pair.command)
		if err != nil {
			return Bundle{}, unavailable("%s is not on PATH: %v", pair.command, err)
		}
		version, err := probeVersion(path)
		if err != nil {
			return Bundle{}, unavailable("%v", err)
		}
		tool, err := toolFromPath(pair.name, path, version)
		if err != nil {
			return Bundle{}, unavailable("pin %s identity: %v", pair.name, err)
		}
		tools = append(tools, tool)
		switch pair.name {
		case "rust-analyzer":
			rustAnalyzerVersion = version
		case "rustc":
			rustcVersion = version
		}
	}
	libRS := "pub trait Render {\n    fn render(&self) -> u32;\n}\n\npub struct Widget;\n\nimpl Render for Widget {\n    fn render(&self) -> u32 {\n        1\n    }\n}\n\npub fn use_it(item: &Widget) -> u32 {\n    item.render()\n}\n"
	if extra {
		libRS += "\npub fn spare_marker() -> u32 {\n    9\n}\n"
	}
	files := map[string][]byte{
		"Cargo.toml": []byte("[package]\nname = \"sevenlang-rust\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"),
		"Cargo.lock": []byte("version = 4\n\n[[package]]\nname = \"sevenlang-rust\"\nversion = \"0.1.0\"\n"),
		"src/lib.rs": []byte(libRS),
	}
	v, err := newView(dir, "rust", files)
	if err != nil {
		return Bundle{}, err
	}
	scope := model.Scope{
		ID:       "sevenlang-rust",
		Language: "rust",
		RootURI:  strings.TrimSuffix(v.rootURI, "/"),
		Build: model.BuildInputs{
			// Features stays nil: the semantic sink clones scopes before
			// recomputing the build context, and cloneScopes normalizes empty
			// slices to nil, so an empty non-nil Features would change the
			// recomputed context identity (JSON [] vs null).
			Options: map[string]string{
				"procMacros":          "false",
				"buildScripts":        "false",
				"cargo.targetSrcPath": "src/lib.rs",
			},
			Tests: true,
		},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      v.workspaceIdentity(),
		Scope:         scope,
		Extractor:     "rust-analyzer-scip",
		ExtractorVer:  rustAnalyzerVersion + ";omnilsp-semantic-model-v3",
		Toolchain:     rustcVersion,
		Backend:       identity.BackendID{Language: "rust", Name: "rust-analyzer"},
		Tools:         tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{
		View:       v,
		Scopes:     []model.Scope{scope},
		Provenance: map[string]model.Provenance{scope.ID: provenance},
	}
	expectation := Expectation{
		SymbolNames:    []string{"Render", "Widget", "use_it"},
		ExtraSymbol:    "spare_marker",
		MinOccurrences: 4,
		MinDefinitions: 3,
		// Plain rust-analyzer SCIP (no patched semantic helper) emits zero
		// relationship entries — verified by decoding its index — so no typed
		// edges are required here; relations land only via the helper whose
		// differential is pinned by rustanalyzer's own locked test.
		EdgeKinds: map[model.EdgeKind]int{},
		Coverage: map[model.FactKind]model.Completeness{
			model.FactImplementation: model.IncompleteKnownSubset,
			model.FactTypeRelation:   model.IncompleteKnownSubset,
		},
	}
	notes := []string{
		"call/import/include/module/generated coverage stays unknown (SCIP does not encode this fact family), exactly as rustanalyzer's own coverage tests pin",
		"plain rust-analyzer SCIP emits no relationships; implementation/type_relation edges require the patched rust-analyzer semantic helper (scip-relations.patch), whose real differential is pinned by the rustanalyzer package's locked test on this host",
	}
	// ExportIndex never consults the nested LSP bridge, so the backend's
	// rust-analyzer process is parked on an empty work directory: pointed at
	// the fixture it would start loading the workspace concurrently and
	// rewrite Cargo.lock under the immutable snapshot the SCIP exporter
	// verifies (observed: "materialized snapshot changed during SCIP export").
	lspWorkDir, err := os.MkdirTemp("", "sevenlang-rust-lsp-")
	if err != nil {
		return Bundle{}, err
	}
	backend, err := rustanalyzer.New(lspWorkDir)
	if err != nil {
		_ = os.RemoveAll(lspWorkDir)
		return Bundle{}, unavailable("rust-analyzer backend unavailable: %v", err)
	}
	return Bundle{
		Language: "rust",
		View:     v,
		Request:  request,
		Export:   backend.ExportIndex,
		Close: func() error {
			closeErr := backend.Close()
			_ = os.RemoveAll(lspWorkDir)
			return closeErr
		},
		Expect: expectation,
		Notes:  notes,
	}, nil
}

// ---------------------------------------------------------------------------
// typescript / javascript (shared direct-compiler exporter)
// ---------------------------------------------------------------------------

func buildTypeScript(ctx context.Context, dir string, extra bool, language string) (Bundle, error) {
	node, err := nodeTool()
	if err != nil {
		return Bundle{}, err
	}
	root, err := repoRoot()
	if err != nil {
		return Bundle{}, err
	}
	compilerPath := filepath.Join(root, "test", "acceptance", "tools", "node_modules", "typescript", "lib", "typescript.js")
	if err := requireFile(compilerPath, "pinned TypeScript 6.0.3 bundle"); err != nil {
		return Bundle{}, err
	}
	compiler, err := toolFromPath("typescript", compilerPath, "6.0.3")
	if err != nil {
		return Bundle{}, err
	}
	exporter, err := hostExecutableTool("omnilsp-typescript-compiler-exporter", "8")
	if err != nil {
		return Bundle{}, err
	}
	tools := []model.ToolIdentity{node, compiler, exporter}

	// Mirror internal/languages/typescript's direct-runner fixtures: the scope
	// options carry the config digest and canonical compiler options, the
	// provenance pins the real node + compiler + embedded exporter identities.
	const tsOptions = `{"target":"ES2020","strict":true}`
	const jsOptions = `{"target":"ES2020","allowJs":true,"checkJs":true}`

	var configBody string
	var useBody string
	var options string
	var configKey string
	var configOptionKey string
	switch language {
	case "typescript":
		configKey, configOptionKey, options = "tsconfig.json", "tsconfig", tsOptions
		configBody = `{"compilerOptions":` + tsOptions + `,"include":["*.ts"]}`
		useBody = "import { Base } from './base';\n\nexport class Child implements Base {\n    run(): void {}\n}\n\nexport function make(): Base {\n    return new Child();\n}\n"
		if extra {
			useBody += "\nexport function spare(): number {\n    return 3;\n}\n"
		}
	case "javascript":
		configKey, configOptionKey, options = "jsconfig.json", "jsconfig", jsOptions
		configBody = `{"compilerOptions":` + jsOptions + `,"include":["*.js"]}`
		useBody = "export class Box {}\n\nexport function identity(item) {\n    return item;\n}\n\nconst boxed = new Box();\nidentity(boxed);\n"
		if extra {
			useBody += "\nfunction spare() {\n    return 3;\n}\n"
		}
	}
	baseName := "base.ts"
	useName := "use.ts"
	if language == "javascript" {
		baseName, useName = "lib.js", "main.js"
	}
	baseBody := "export interface Base {\n    run(): void;\n}\n"
	if language == "javascript" {
		baseBody = "export class Base {}\n"
	}

	files := map[string][]byte{
		configKey: []byte(configBody),
		baseName:  []byte(baseBody),
		useName:   []byte(useBody),
	}
	v, err := newView(dir, language, files)
	if err != nil {
		return Bundle{}, err
	}
	configDigest := sha256.Sum256(files[configKey])
	scope := model.Scope{
		ID:       "sevenlang-" + language,
		Language: language,
		RootURI:  strings.TrimSuffix(v.rootURI, "/"),
		Build: model.BuildInputs{Options: map[string]string{
			configOptionKey:            configKey,
			configOptionKey + "Digest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions":          options,
			"projectReferences":        "[]",
			"plugins":                  "[]",
		}},
	}
	toolchain := "typescript/6.0.3"
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      v.workspaceIdentity(),
		Scope:         scope,
		Extractor:     "omnilsp-typescript-scip",
		ExtractorVer:  "8",
		Toolchain:     toolchain,
		Backend:       identity.BackendID{Language: language, Name: "typescript-direct-compiler"},
		Tools:         tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{
		View:       v,
		Scopes:     []model.Scope{scope},
		Provenance: map[string]model.Provenance{scope.ID: provenance},
	}
	provider := typescript.NewSemanticIndexProvider(typescript.SemanticIndexConfig{
		Runner: typescript.NewDirectCompilerRunner("node"),
		Tools:  tools,
	})
	expectation := Expectation{
		SymbolNames:    []string{"Base", "Child", "make"},
		ExtraSymbol:    "spare",
		MinOccurrences: 4,
		MinDefinitions: 2,
		EdgeKinds: map[model.EdgeKind]int{
			model.EdgeImplementation: 1,
			model.EdgeImport:         1,
			model.EdgeTypeRelation:   1,
		},
		Coverage: map[model.FactKind]model.Completeness{
			model.FactInclude: model.Unavailable,
		},
	}
	if language == "javascript" {
		expectation = Expectation{
			SymbolNames:    []string{"Base", "Box", "identity"},
			ExtraSymbol:    "spare",
			MinOccurrences: 4,
			MinDefinitions: 2,
			EdgeKinds: map[model.EdgeKind]int{
				model.EdgeCall: 1,
			},
		}
	}
	return Bundle{
		Language: language,
		View:     v,
		Request:  request,
		Export:   provider.ExportIndex,
		Expect:   expectation,
	}, nil
}

// ---------------------------------------------------------------------------
// python (pinned Pyright direct runner)
// ---------------------------------------------------------------------------

func buildPython(ctx context.Context, dir string, extra bool) (Bundle, error) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return Bundle{}, unavailable("node is not on PATH: %v", err)
	}
	nodeVersion, err := probeVersion(nodePath)
	if err != nil {
		return Bundle{}, unavailable("%v", err)
	}
	nodeVersion = strings.TrimPrefix(nodeVersion, "v")
	if nodeVersion != pyrightPinnedNodeVersion {
		return Bundle{}, unavailable("node reports %s, the audited Pyright bundle pins %s", nodeVersion, pyrightPinnedNodeVersion)
	}
	node, err := toolFromPath("node", nodePath, nodeVersion)
	if err != nil {
		return Bundle{}, err
	}
	root, err := repoRoot()
	if err != nil {
		return Bundle{}, err
	}
	pyrightRoot := filepath.Join(root, "test", "acceptance", "tools", "node_modules", "pyright")
	internalPath := filepath.Join(pyrightRoot, "dist", "pyright-internal.js")
	vendorPath := filepath.Join(pyrightRoot, "dist", "vendor.js")
	if err := requireFile(internalPath, "pinned Pyright analyzer bundle"); err != nil {
		return Bundle{}, err
	}
	if err := requireFile(vendorPath, "pinned Pyright vendor bundle"); err != nil {
		return Bundle{}, err
	}
	compiler, err := toolFromPath("pyright", internalPath, pyrightCompilerVersion)
	if err != nil {
		return Bundle{}, err
	}
	vendor, err := toolFromPath("pyright-vendor", vendorPath, pyrightCompilerVersion)
	if err != nil {
		return Bundle{}, err
	}
	exporter, err := hostExecutableTool("omnilsp-pyright-exporter", pyrightExtractorVersion)
	if err != nil {
		return Bundle{}, err
	}
	pythonPath, err := exec.LookPath("python")
	if err != nil {
		pythonPath, err = exec.LookPath("python3")
	}
	if err != nil {
		return Bundle{}, unavailable("python interpreter is not on PATH: %v", err)
	}
	pythonVersion, err := probeVersion(pythonPath)
	if err != nil {
		return Bundle{}, unavailable("%v", err)
	}
	versionFields := strings.Fields(pythonVersion)
	if len(versionFields) == 0 {
		return Bundle{}, unavailable("python --version returned no fields")
	}
	python, err := toolFromPath("python", pythonPath, versionFields[len(versionFields)-1])
	if err != nil {
		return Bundle{}, err
	}
	tools := []model.ToolIdentity{node, compiler, vendor, exporter, python}

	usePy := "from pkg.base import Base\n\n\nclass Child(Base):\n    def call(self, value: Base) -> int:\n        return value.ping()\n"
	if extra {
		usePy += "\n\ndef spare():\n    return 3\n"
	}
	files := map[string][]byte{
		"pyrightconfig.json": []byte(`{"typeCheckingMode":"strict"}`),
		"pkg/base.py":        []byte("class Base:\n    def ping(self) -> int:\n        return 1\n"),
		"pkg/use.py":         []byte(usePy),
	}
	v, err := newView(dir, "python", files)
	if err != nil {
		return Bundle{}, err
	}
	provider := pyright.NewPinnedSemanticIndexProvider(tools)
	request, err := provider.BuildIndexRequest(ctx, v, strings.TrimSuffix(v.rootURI, "/"))
	if err != nil {
		return Bundle{}, err
	}
	expectation := Expectation{
		SymbolNames:    []string{"Base", "Child", "call", "ping"},
		ExtraSymbol:    "spare",
		MinOccurrences: 4,
		MinDefinitions: 2,
		EdgeKinds: map[model.EdgeKind]int{
			model.EdgeImplementation: 1,
			model.EdgeCall:           1,
		},
		Coverage: map[model.FactKind]model.Completeness{
			model.FactInclude: model.Unavailable,
		},
	}
	return Bundle{
		Language: "python",
		View:     v,
		Request:  request,
		Export:   provider.ExportIndex,
		Expect:   expectation,
	}, nil
}

// ---------------------------------------------------------------------------
// cpp / c (ccls backend over the pinned libclang extractor)
// ---------------------------------------------------------------------------

func pinnedLLVMHome() string {
	if home := strings.TrimSpace(os.Getenv("OMNILSP_LLVM22_HOME")); home != "" {
		return home
	}
	return `D:\Program Files\LLVM-22\clang+llvm-22.1.5-x86_64-pc-windows-msvc`
}

func buildClang(ctx context.Context, dir string, extra bool, language string) (Bundle, error) {
	if runtime.GOOS != "windows" {
		return Bundle{}, unavailable("the pinned LLVM 22.1.5 layout is Windows-only on this evidence host")
	}
	compilerName := "clang++.exe"
	if language == "c" {
		compilerName = "clang.exe"
	}
	compilerPath := filepath.Join(pinnedLLVMHome(), "bin", compilerName)
	if err := requireFile(compilerPath, "pinned LLVM 22.1.5 "+compilerName); err != nil {
		return Bundle{}, err
	}

	apiHpp := "#pragma once\nstruct Base {\n    virtual int value(int) const = 0;\n};\nstruct Widget : Base {\n    int value(int) const override;\n    int score(int) const;\n};\n"
	apiCpp := "#include \"api.hpp\"\nint Widget::value(int x) const { return x + 1; }\nint Widget::score(int x) const { return x + 2; }\n"
	mainCpp := "#include \"api.hpp\"\nint run(Widget& w) { return w.value(1) + w.score(2); }\n"
	packetH := "#pragma once\nstruct Packet {\n    int size;\n};\nint packet_send(const struct Packet* packet);\n"
	packetC := "#include \"packet.h\"\nint packet_send(const struct Packet* packet) {\n    return packet->size;\n}\n"
	mainC := "#include \"packet.h\"\nstatic int boot(void) {\n    struct Packet item;\n    item.size = 1;\n    return packet_send(&item);\n}\n"

	var files map[string][]byte
	var expectation Expectation
	notes := []string{}
	switch language {
	case "cpp":
		if extra {
			mainCpp += "\nint spare(int x) { return x; }\n"
		}
		files = map[string][]byte{"api.hpp": []byte(apiHpp), "api.cpp": []byte(apiCpp), "main.cpp": []byte(mainCpp)}
		expectation = Expectation{
			SymbolNames:    []string{"Base", "Widget"},
			ExtraSymbol:    "spare",
			MinOccurrences: 5,
			MinDefinitions: 2,
			EdgeKinds: map[model.EdgeKind]int{
				model.EdgeImplementation: 1,
				model.EdgeTypeRelation:   1,
				model.EdgeInclude:        1,
				model.EdgeCall:           1,
			},
			Coverage: map[model.FactKind]model.Completeness{
				model.FactInclude:      model.Complete,
				model.FactTypeRelation: model.IncompleteKnownSubset,
			},
		}
	case "c":
		if extra {
			packetC += "\nint spare(void) {\n    return 3;\n}\n"
		}
		files = map[string][]byte{"packet.h": []byte(packetH), "packet.c": []byte(packetC), "main.c": []byte(mainC)}
		expectation = Expectation{
			SymbolNames:    []string{"Packet", "packet_send"},
			ExtraSymbol:    "spare",
			MinOccurrences: 4,
			MinDefinitions: 1,
			EdgeKinds: map[model.EdgeKind]int{
				model.EdgeInclude: 1,
				model.EdgeCall:    1,
			},
			Coverage: map[model.FactKind]model.Completeness{
				model.FactInclude: model.Complete,
			},
		}
		notes = append(notes,
			"the libclang extractor pins scope language \"cpp\" for every compile context (build_request.go), so the c alias rides the cpp exporter until the server-side c key is registered",
			"the server-side c alias key stays X10-gated; this chain proves the c translation unit's typed facts through the real extractor, not a registered c lane")
	}

	// compile_commands.json references absolute fixture paths with argument
	// arrays, mirroring the ccls coverage fixture.
	rootPath, err := filepath.Abs(dir)
	if err != nil {
		return Bundle{}, err
	}
	type dbEntry struct {
		Directory string   `json:"directory"`
		File      string   `json:"file"`
		Arguments []string `json:"arguments"`
	}
	var entries []dbEntry
	for _, file := range []string{"api.cpp", "main.cpp"} {
		if _, ok := files[file]; !ok {
			continue
		}
		entries = append(entries, dbEntry{
			Directory: rootPath,
			File:      filepath.Join(rootPath, file),
			Arguments: []string{compilerPath, "-std=c++17", "-I", rootPath, "-c", filepath.Join(rootPath, file), "-o", filepath.Join(rootPath, "obj", strings.TrimSuffix(file, ".cpp")+".obj")},
		})
	}
	for _, file := range []string{"packet.c", "main.c"} {
		if _, ok := files[file]; !ok {
			continue
		}
		entries = append(entries, dbEntry{
			Directory: rootPath,
			File:      filepath.Join(rootPath, file),
			Arguments: []string{compilerPath, "-I", rootPath, "-c", filepath.Join(rootPath, file), "-o", filepath.Join(rootPath, "obj", strings.TrimSuffix(file, ".c")+".obj")},
		})
	}
	db, err := json.Marshal(entries)
	if err != nil {
		return Bundle{}, err
	}
	files["compile_commands.json"] = db

	v, err := newView(dir, language, files)
	if err != nil {
		return Bundle{}, err
	}
	// ccls.New spawns the nested clangd bridge (on PATH) but the index surface
	// never consults it; the fixture proves the same production constructor.
	backend, err := ccls.New(v.rootPath)
	if err != nil {
		return Bundle{}, unavailable("ccls backend (clangd bridge) unavailable: %v", err)
	}
	request, err := backend.BuildIndexRequest(ctx, v, strings.TrimSuffix(v.rootURI, "/"))
	if err != nil {
		_ = backend.Close()
		return Bundle{}, err
	}
	return Bundle{
		Language: language,
		View:     v,
		Request:  request,
		Export:   backend.ExportIndex,
		Close:    backend.Close,
		Expect:   expectation,
		Notes:    notes,
	}, nil
}

// nodeTool pins the host Node executable with its real version.
func nodeTool() (model.ToolIdentity, error) {
	path, err := exec.LookPath("node")
	if err != nil {
		return model.ToolIdentity{}, unavailable("node is not on PATH: %v", err)
	}
	version, err := probeVersion(path)
	if err != nil {
		return model.ToolIdentity{}, unavailable("%v", err)
	}
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return model.ToolIdentity{}, unavailable("node --version returned an empty version")
	}
	return toolFromPath("node", path, version)
}
