package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/fileio"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	uriutil "github.com/omnilsp/omni/internal/workspace/uri"
	"golang.org/x/tools/go/packages"
)

type packageCacheEntry struct {
	pkg              *packages.Package
	inputFingerprint identity.ContentHash
}

type workspaceOverlayContextKey struct{}

type packageOverlays struct {
	byPath map[string]workspaceOverlayFile
}

type workspaceOverlayFile struct {
	path    string
	content []byte
}

func (overlays packageOverlays) goPackagesOverlay() map[string][]byte {
	if len(overlays.byPath) == 0 {
		return nil
	}
	result := make(map[string][]byte, len(overlays.byPath))
	for _, file := range overlays.byPath {
		result[file.path] = file.content
	}
	return result
}

func (overlays packageOverlays) withFile(path string, content []byte) (packageOverlays, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return packageOverlays{}, err
	}
	path = filepath.Clean(path)
	result := packageOverlays{byPath: make(map[string]workspaceOverlayFile, len(overlays.byPath)+1)}
	for key, file := range overlays.byPath {
		result.byPath[key] = file
	}
	result.byPath[pathKey(path)] = workspaceOverlayFile{
		path: path, content: cloneContent(content),
	}
	return result, nil
}

func packageOverlaysFromContext(ctx context.Context) packageOverlays {
	if ctx == nil {
		return packageOverlays{}
	}
	overlays, _ := ctx.Value(workspaceOverlayContextKey{}).(packageOverlays)
	return overlays
}

func packageOverlaysFromWorkspace(snapshot languages.WorkspaceSnapshot) (packageOverlays, error) {
	byPath := make(map[string]workspaceOverlayFile, len(snapshot.Documents))
	for _, document := range snapshot.Documents {
		parsed, err := uriutil.Parse(document.URI)
		if err != nil || !parsed.IsFile() {
			continue
		}
		path, err := parsed.Path()
		if err != nil {
			return packageOverlays{}, fmt.Errorf("resolve workspace document %q: %w", document.URI, err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return packageOverlays{}, fmt.Errorf("resolve workspace document %q: %w", document.URI, err)
		}
		path = filepath.Clean(path)
		key := pathKey(path)
		content := cloneContent(document.Content)
		if previous, exists := byPath[key]; exists {
			if !equalBytes(previous.content, content) {
				return packageOverlays{}, fmt.Errorf("workspace snapshot maps multiple documents to %q", path)
			}
			continue
		}
		byPath[key] = workspaceOverlayFile{path: path, content: content}
	}
	return packageOverlays{byPath: byPath}, nil
}

func packageOverlaysFromSnapshot(captured *snapshot.Snapshot) (packageOverlays, error) {
	if captured == nil {
		return packageOverlays{}, nil
	}
	uris := captured.Documents()
	documents := make([]languages.WorkspaceDocument, 0, len(uris))
	for _, documentURI := range uris {
		document := captured.Document(documentURI)
		if document == nil {
			continue
		}
		documents = append(documents, languages.WorkspaceDocument{
			URI: documentURI, LanguageID: document.LanguageID,
			Version: document.Version, Content: document.Content,
		})
	}
	return packageOverlaysFromWorkspace(languages.WorkspaceSnapshot{
		Revision: captured.ID().Revision, Documents: documents,
	})
}

// BeginWorkspaceSnapshot binds immutable overlays to the request context. Go
// package loading is in-process, so it does not need a cross-request lease or
// backend-wide lock; each request carries the exact workspace view it uses.
func (b *Backend) BeginWorkspaceSnapshot(ctx context.Context, workspace languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	overlays, err := packageOverlaysFromWorkspace(workspace)
	if err != nil {
		return nil, nil, err
	}
	leaseCtx := context.WithValue(ctx, workspaceOverlayContextKey{}, overlays)
	var once sync.Once
	var finishErr error
	finish := func() error {
		once.Do(func() { finishErr = ctx.Err() })
		return finishErr
	}
	return leaseCtx, finish, nil
}

// WorkspaceSnapshotGeneration is constant because snapshots are immutable and
// the backend has no separately mutable workspace document state.
func (b *Backend) WorkspaceSnapshotGeneration() uint64 { return 0 }

// SemanticInputFingerprint returns an identity only when a previously loaded
// package has a complete, verifiable input closure. Empty identity means the
// root semantic memo must call the backend instead of caching its result.
func (b *Backend) SemanticInputFingerprint(ctx context.Context, captured *snapshot.Snapshot, uri string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if captured == nil {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parsed, err := uriutil.Parse(uri)
	if err != nil || !parsed.IsFile() {
		return "", fmt.Errorf("Go semantic input fingerprint requires a file URI: %q", uri)
	}
	filePath, err := parsed.Path()
	if err != nil {
		return "", err
	}
	filePath, err = filepath.Abs(filePath)
	if err != nil {
		return "", err
	}
	filePath = filepath.Clean(filePath)
	content := []byte(nil)
	overlays, err := packageOverlaysFromSnapshot(captured)
	if err != nil {
		return "", err
	}
	if document := captured.Document(uri); document != nil {
		content = document.Content
		overlays, err = overlays.withFile(filePath, content)
		if err != nil {
			return "", err
		}
	} else if file, ok := overlays.byPath[pathKey(filePath)]; ok {
		content = file.content
	} else {
		content, err = fileio.ReadFileShared(filePath)
		if err != nil {
			return "", fmt.Errorf("read Go semantic request source %q: %w", filePath, err)
		}
	}
	buildContext := b.BuildContextID()
	key := packageCacheKey(filePath, captured.ID().Revision, content, overlays, buildContext)
	if err := b.lock(ctx); err != nil {
		return "", err
	}
	defer b.unlock()
	entry, ok := b.pkgCache[key]
	if !ok || entry.pkg == nil {
		return "", nil
	}
	fingerprint, complete, err := packageInputFingerprint(ctx, entry.pkg, filePath, overlays, buildContext, b.goWorkPath, b.goFlags)
	if err != nil {
		return "", err
	}
	if !complete {
		return "", nil
	}
	return "go-package:" + string(fingerprint), nil
}

func packageCacheKey(filePath string, snapshotRev uint64, content []byte, overlays packageOverlays, buildContext identity.BuildContextID) string {
	contentHash := hashContent(content)
	return fmt.Sprintf("%s|%d|%s|%s", pathKey(filePath), snapshotRev, contentHash, buildContext)
}

func hashContent(content []byte) identity.ContentHash {
	sum := sha256.Sum256(content)
	return contentHashFromBytes(sum[:])
}

func contentHashFromBytes(sum []byte) identity.ContentHash {
	return identity.ContentHash("sha256:" + hex.EncodeToString(sum))
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func cloneContent(content []byte) []byte {
	clone := make([]byte, len(content))
	copy(clone, content)
	return clone
}

type packageInputFile struct {
	path     string
	optional bool
}

func packageInputFingerprint(ctx context.Context, root *packages.Package, targetPath string, overlays packageOverlays, buildContext identity.BuildContextID, goWorkPath, goFlags string) (identity.ContentHash, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if root == nil || buildContext == "" || buildContext == "go:sha256:unavailable" {
		return "", false, nil
	}
	if goFlagsContainUntrackedInputs(goFlags) {
		return "", false, nil
	}

	packagesSeen := make(map[*packages.Package]struct{})
	queue := []*packages.Package{root}
	inputFiles := make(map[string]packageInputFile)
	directories := make(map[string]string)
	packageDirs := make([]string, 0, 8)
	var embedPatterns []string
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		pkg := queue[0]
		queue = queue[1:]
		if pkg == nil {
			return "", false, nil
		}
		if _, seen := packagesSeen[pkg]; seen {
			continue
		}
		packagesSeen[pkg] = struct{}{}
		if !packageErrorsCacheable(pkg) {
			return "", false, nil
		}
		if pkg.Dir != "" {
			dir, err := filepath.Abs(pkg.Dir)
			if err != nil {
				return "", false, err
			}
			dir = filepath.Clean(dir)
			directories[pathKey(dir)] = dir
			packageDirs = append(packageDirs, dir)
		}
		for _, files := range [][]string{pkg.GoFiles, pkg.CompiledGoFiles, pkg.OtherFiles, pkg.EmbedFiles, pkg.IgnoredFiles} {
			for _, path := range files {
				if path == "" || strings.HasPrefix(path, "<") {
					return "", false, nil
				}
				if err := addPackageInputFile(inputFiles, path, false); err != nil {
					return "", false, err
				}
			}
		}
		for _, pattern := range pkg.EmbedPatterns {
			pattern = strings.TrimPrefix(pattern, "all:")
			if !filepath.IsAbs(pattern) && pkg.Dir != "" {
				pattern = filepath.Join(pkg.Dir, pattern)
			}
			embedPatterns = append(embedPatterns, pattern)
			matches, err := filepath.Glob(pattern)
			if err != nil {
				return "", false, nil
			}
			for _, path := range matches {
				if complete, err := addPackageEmbedMatch(ctx, inputFiles, directories, path); err != nil {
					return "", false, err
				} else if !complete {
					return "", false, nil
				}
			}
		}
		for _, file := range pkg.Syntax {
			if file == nil {
				return "", false, nil
			}
			for _, imported := range file.Imports {
				importPath, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return "", false, nil
				}
				if importPath == "C" {
					// C headers can be outside the package metadata's closed file set.
					return "", false, nil
				}
				if pkg.Imports[importPath] == nil {
					return "", false, nil
				}
			}
		}
		for _, dependency := range pkg.Imports {
			if dependency == nil {
				return "", false, nil
			}
			queue = append(queue, dependency)
		}
		if pkg.Module != nil && pkg.Module.GoMod != "" {
			if err := addPackageInputFile(inputFiles, pkg.Module.GoMod, false); err != nil {
				return "", false, err
			}
			moduleDir := filepath.Dir(pkg.Module.GoMod)
			for _, name := range []string{"go.sum", filepath.Join("vendor", "modules.txt")} {
				if err := addPackageInputFile(inputFiles, filepath.Join(moduleDir, name), true); err != nil {
					return "", false, err
				}
			}
		}
	}

	if goWorkPath != "" && goWorkPath != "off" {
		if err := addPackageInputFile(inputFiles, goWorkPath, false); err != nil {
			return "", false, err
		}
		workDir := filepath.Dir(goWorkPath)
		if err := addPackageInputFile(inputFiles, filepath.Join(workDir, "go.work.sum"), true); err != nil {
			return "", false, err
		}
	}
	if err := addPackageInputFile(inputFiles, targetPath, false); err != nil {
		return "", false, err
	}
	if len(packageDirs) == 0 {
		return "", false, nil
	}
	for key, file := range overlays.byPath {
		if isPackageOverlayInput(file.path, key, packageDirs, embedPatterns, inputFiles) {
			inputFiles[key] = packageInputFile{path: file.path}
		}
	}

	inputHash := sha256.New()
	writeHashField(inputHash, string(buildContext))
	writeHashField(inputHash, pathKey(targetPath))
	fileKeys := make([]string, 0, len(inputFiles))
	for key := range inputFiles {
		fileKeys = append(fileKeys, key)
	}
	sort.Strings(fileKeys)
	for _, key := range fileKeys {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		file := inputFiles[key]
		digest, exists, err := hashPackageInputFile(ctx, file, overlays)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return "", false, nil
		}
		writeHashField(inputHash, key)
		writeHashField(inputHash, string(digest))
	}
	directoryKeys := make([]string, 0, len(directories))
	for key := range directories {
		directoryKeys = append(directoryKeys, key)
	}
	sort.Strings(directoryKeys)
	for _, key := range directoryKeys {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		digest, exists, err := hashPackageDirectory(ctx, directories[key], overlays)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return "", false, nil
		}
		writeHashField(inputHash, key)
		writeHashField(inputHash, string(digest))
	}
	return contentHashFromBytes(inputHash.Sum(nil)), true, nil
}

// packageErrorsCacheable reports whether a package's load errors leave a graph
// that a matching re-load would reproduce, so the package may back a cache
// entry keyed by the input-closure fingerprint. Layout-class faults
// (packages.ListError — go list verdicts such as "C source files not allowed
// when not using cgo or SWIG") are deterministic functions of the inputs the
// fingerprint already hashes: the package directory listing
// (hashPackageDirectory), the Go file contents, and the module/workspace
// files. Type and parse failures are genuine semantic faults, module-graph
// errors can resolve through module-cache state the closure never hashes, and
// UnknownError is unclassifiable, so all of those fail closed, as does
// IllTyped without any local error (an ill-typed dependency outside the
// visited Imports closure). Known limitation: a ListError verdict that
// depends on CGO_ENABLED can go stale if the toolchain setting flips
// mid-session via `go env -w`; the backend derives its environment once (as
// it already does for GOFLAGS/GOWORK), and such a stale entry serves the same
// Go graph with under-claimed (partial) evidence, never fabricated semantics.
func packageErrorsCacheable(pkg *packages.Package) bool {
	if pkg.Module != nil && pkg.Module.Error != nil {
		return false
	}
	if len(pkg.Errors) == 0 {
		return !pkg.IllTyped
	}
	for _, err := range pkg.Errors {
		if err.Kind != packages.ListError {
			return false
		}
	}
	return true
}

func addPackageInputFile(files map[string]packageInputFile, path string, optional bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	path = filepath.Clean(path)
	key := pathKey(path)
	previous, exists := files[key]
	if !exists || (!optional && previous.optional) {
		files[key] = packageInputFile{path: path, optional: optional}
	}
	return nil
}

func addPackageEmbedMatch(ctx context.Context, files map[string]packageInputFile, directories map[string]string, match string) (bool, error) {
	info, err := os.Stat(match)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat Go embed input %q: %w", match, err)
	}
	if !info.IsDir() {
		if err := addPackageInputFile(files, match, false); err != nil {
			return false, err
		}
		return true, addPackageInputDirectory(directories, filepath.Dir(match))
	}
	walkErr := filepath.WalkDir(match, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return addPackageInputDirectory(directories, path)
		}
		return addPackageInputFile(files, path, false)
	})
	if walkErr != nil {
		if os.IsNotExist(walkErr) {
			return false, nil
		}
		return false, fmt.Errorf("walk Go embed input %q: %w", match, walkErr)
	}
	return true, nil
}

func addPackageInputDirectory(directories map[string]string, directory string) error {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	directory = filepath.Clean(directory)
	directories[pathKey(directory)] = directory
	return nil
}

func hashPackageInputFile(ctx context.Context, file packageInputFile, overlays packageOverlays) (identity.ContentHash, bool, error) {
	path, err := filepath.Abs(file.path)
	if err != nil {
		return "", false, err
	}
	key := pathKey(filepath.Clean(path))
	if overlay, ok := overlays.byPath[key]; ok {
		return hashContent(overlay.content), true, nil
	}
	input, err := fileio.OpenReadShared(path)
	if err != nil {
		if file.optional && os.IsNotExist(err) {
			return hashContent([]byte("missing")), true, nil
		}
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("open Go package input %q: %w", path, err)
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, r: input}); err != nil {
		return "", false, fmt.Errorf("hash Go package input %q: %w", path, err)
	}
	return contentHashFromBytes(hash.Sum(nil)), true, nil
}

func hashPackageDirectory(ctx context.Context, directory string, overlays packageOverlays) (identity.ContentHash, bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read Go package directory %q: %w", directory, err)
	}
	types := make(map[string]string, len(entries)+len(overlays.byPath))
	for _, entry := range entries {
		types[entry.Name()] = entry.Type().String()
		if entry.IsDir() {
			types[entry.Name()] = "directory"
		}
	}
	for _, file := range overlays.byPath {
		if pathKey(filepath.Dir(file.path)) == pathKey(directory) {
			types[filepath.Base(file.path)] = "overlay"
		}
	}
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		writeHashField(hash, name)
		writeHashField(hash, types[name])
	}
	return contentHashFromBytes(hash.Sum(nil)), true, nil
}

func isPackageOverlayInput(path, key string, directories []string, embedPatterns []string, inputs map[string]packageInputFile) bool {
	if _, ok := inputs[key]; ok {
		return true
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	for _, directory := range directories {
		if pathKey(filepath.Dir(path)) == pathKey(directory) {
			return true
		}
	}
	candidate := filepath.ToSlash(path)
	for _, pattern := range embedPatterns {
		pattern = strings.TrimPrefix(pattern, "all:")
		pattern = filepath.ToSlash(pattern)
		for candidatePath := candidate; ; candidatePath = filepath.ToSlash(filepath.Dir(filepath.FromSlash(candidatePath))) {
			if matched, err := filepath.Match(pattern, candidatePath); err == nil && matched {
				return true
			}
			candidateDir := filepath.FromSlash(candidatePath)
			if filepath.Dir(candidateDir) == candidateDir {
				break
			}
		}
	}
	return false
}

func writeHashField(hash io.Writer, value string) {
	_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
	_, _ = io.WriteString(hash, ":")
	_, _ = io.WriteString(hash, value)
}

func goFlagsContainUntrackedInputs(value string) bool {
	for _, flag := range strings.Fields(value) {
		name := strings.SplitN(flag, "=", 2)[0]
		switch name {
		case "-modfile", "-overlay", "-toolexec", "-exec", "-pkgdir", "-workfile":
			return true
		}
	}
	return false
}
