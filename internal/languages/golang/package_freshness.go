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

// round23 C: the package-cache hit path recomputes the whole input-closure
// fingerprint on every request (loadPackage's hit branch), and the measured
// cost is dominated by re-opening and SHA-256-ing every input file — for a
// single `import "fmt"` main with a 65-package / 529-file closure, one
// fingerprint scan costs ~160ms, which is essentially the entire hit-path
// cost. The digest cache memoizes per-file digests keyed by
// (path, size, mtime): an unchanged file costs one directory-listing stat
// instead of a full read+hash, and only changed files are re-hashed.
//
// Scope decision (round23 C, measured): reuse is restricted to
// toolchain-managed roots (GOROOT, GOMODCACHE). The package fingerprint is
// pinned by TestPackageCacheRevalidatesSameSizeSameModTimeDependencyEdit to
// catch workspace dependency edits that preserve BOTH size and mtime — a
// stat-keyed memo cannot see those, so user-editable workspace files always
// re-hash at full strength. GOROOT and the module cache are owned by the
// toolchain (module-cache files are read-only, go.sum-verified on download),
// and they are essentially the whole measured closure cost: the fmt closure
// is 528 GOROOT files plus one workspace file. Residual, disclosed trade: a
// deliberate same-size-same-mtime rewrite inside those roots would reuse the
// old digest where today's full hash would catch it.
type packageDigestCache struct {
	mu    sync.Mutex
	roots []string // pathKey'd toolchain roots; empty = memo inert
	byKey map[digestCacheKey]identity.ContentHash
}

type digestCacheKey struct {
	path   string // pathKey-normalized
	size   int64
	mtimeN int64 // ModTime().UnixNano()
}

// maxPackageDigestCacheEntries bounds the memo. Overflow drops the whole
// map: correctness is unaffected (a miss re-hashes the file) and the next
// scan repopulates.
const maxPackageDigestCacheEntries = 65536

func newPackageDigestCache() *packageDigestCache {
	return &packageDigestCache{byKey: make(map[digestCacheKey]identity.ContentHash)}
}

// setToolchainRoots scopes the memo to toolchain-managed file roots. Called
// once from deriveBuildContext inside buildCtxOnce, before any fingerprint
// can run — both fingerprint callers derive the build context first, and a
// fingerprint with an unavailable build context never reaches the file loop.
func (c *packageDigestCache) setToolchainRoots(goroot, goModCache string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roots = c.roots[:0]
	for _, root := range []string{goroot, goModCache} {
		if root == "" || root == "off" {
			continue
		}
		c.roots = append(c.roots, pathKey(filepath.Clean(root)))
	}
}

// reusable reports whether the memo may serve a stat-validated digest for
// this path. See the packageDigestCache doc for the scope decision.
func (c *packageDigestCache) reusable(path string) bool {
	c.mu.Lock()
	roots := c.roots
	c.mu.Unlock()
	if len(roots) == 0 {
		return false
	}
	key := pathKey(path)
	for _, root := range roots {
		if key == root {
			return true
		}
		if len(key) > len(root) && strings.HasPrefix(key, root) {
			if sep := key[len(root)]; sep == '\\' || sep == '/' {
				return true
			}
		}
	}
	return false
}

// reuse returns the memoized digest when the file still stats exactly as the
// remembered (size, mtime).
func (c *packageDigestCache) reuse(path string) (identity.ContentHash, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return c.reuseStat(digestCacheKey{path: pathKey(path), size: info.Size(), mtimeN: info.ModTime().UnixNano()})
}

// reuseStat is reuse against a pre-computed stat key — the normal path when
// a directory listing already supplied the (size, mtime) proof.
func (c *packageDigestCache) reuseStat(key digestCacheKey) (identity.ContentHash, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	digest, ok := c.byKey[key]
	return digest, ok
}

// remember pins a digest under the stat taken before the read, only when a
// post-read stat still matches it: a file rewritten between the two stats
// has content that matches neither, and pinning either stat would risk a
// stale digest on a later (size, mtime) match.
func (c *packageDigestCache) remember(path string, before os.FileInfo, digest identity.ContentHash) {
	after, err := os.Stat(path)
	if err != nil || !sameFileStat(before, after) {
		return
	}
	key := digestCacheKey{path: pathKey(path), size: after.Size(), mtimeN: after.ModTime().UnixNano()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byKey) >= maxPackageDigestCacheEntries {
		c.byKey = make(map[digestCacheKey]identity.ContentHash)
	}
	c.byKey[key] = digest
}

func sameFileStat(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// directoryListings captures, with one os.ReadDir per package directory, the
// (name → type) state hashPackageDirectory hashes and the (name → size,
// mtime) state that validates memoized file digests. Measured on this
// codepath, the first cut of the digest cache still spent ~90ms of a warm
// fingerprint on ~1800 per-file os.Stat calls; one listing per directory
// (which the directory hash already paid for) replaces them — on Windows
// DirEntry.Info is satisfied from the FindFirstFile buffer, so the per-file
// metadata costs nothing beyond the single ReadDir.
type directoryListings struct {
	byDir map[string]*directoryListing // pathKey(dir) → listing
}

type directoryListing struct {
	types map[string]string      // name → entry type string, pre-overlay
	stats map[string]os.FileInfo // name → info for regular entries only
}

// readDirectoryListings never fails hard: a directory it cannot read simply
// contributes no listing, and every consumer falls back to per-file
// behavior (the directory hash re-reads and reports; file stats fall back to
// os.Stat).
func readDirectoryListings(ctx context.Context, directories map[string]string) *directoryListings {
	listings := &directoryListings{byDir: make(map[string]*directoryListing, len(directories))}
	for _, dir := range directories {
		if err := ctx.Err(); err != nil {
			return listings
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		listing := &directoryListing{
			types: make(map[string]string, len(entries)),
			stats: make(map[string]os.FileInfo, len(entries)),
		}
		for _, entry := range entries {
			typeStr := entry.Type().String()
			if entry.IsDir() {
				typeStr = "directory"
			}
			listing.types[entry.Name()] = typeStr
			// Regular entries only: a symlink's own listing stat does not
			// reflect the target's changes, and os.Stat (which follows
			// links, matching OpenReadShared) must validate those.
			if entry.Type().IsRegular() {
				if info, infoErr := entry.Info(); infoErr == nil {
					listing.stats[entry.Name()] = info
				}
			}
		}
		listings.byDir[pathKey(dir)] = listing
	}
	return listings
}

func (l *directoryListings) listingFor(directory string) *directoryListing {
	if l == nil {
		return nil
	}
	return l.byDir[pathKey(directory)]
}

func (l *directoryListings) digestKey(path string) (digestCacheKey, bool) {
	listing := l.listingFor(filepath.Dir(path))
	if listing == nil {
		return digestCacheKey{}, false
	}
	info, ok := listing.stats[filepath.Base(path)]
	if !ok {
		return digestCacheKey{}, false
	}
	return digestCacheKey{path: pathKey(path), size: info.Size(), mtimeN: info.ModTime().UnixNano()}, true
}

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
	fingerprint, complete, err := packageInputFingerprint(ctx, entry.pkg, filePath, overlays, buildContext, b.goWorkPath, b.goFlags, b.digests)
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

func packageInputFingerprint(ctx context.Context, root *packages.Package, targetPath string, overlays packageOverlays, buildContext identity.BuildContextID, goWorkPath, goFlags string, digests *packageDigestCache) (identity.ContentHash, bool, error) {
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
	// One listing per package directory serves both the per-file digest
	// validation and the directory hash below.
	listings := readDirectoryListings(ctx, directories)
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
		digest, exists, err := hashPackageInputFile(ctx, file, overlays, digests, listings)
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
		digest, exists, err := hashPackageDirectory(ctx, directories[key], overlays, listings.listingFor(directories[key]))
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

func hashPackageInputFile(ctx context.Context, file packageInputFile, overlays packageOverlays, digests *packageDigestCache, listings *directoryListings) (identity.ContentHash, bool, error) {
	path, err := filepath.Abs(file.path)
	if err != nil {
		return "", false, err
	}
	key := pathKey(filepath.Clean(path))
	if overlay, ok := overlays.byPath[key]; ok {
		return hashContent(overlay.content), true, nil
	}
	if digests != nil && digests.reusable(path) {
		// A directory listing is the cheap (size, mtime) proof when the
		// file's directory was listed for the directory hash anyway;
		// otherwise fall back to a direct stat.
		if statKey, ok := listings.digestKey(path); ok {
			if digest, hit := digests.reuseStat(statKey); hit {
				return digest, true, nil
			}
		} else if digest, hit := digests.reuse(path); hit {
			return digest, true, nil
		}
	}
	before, statErr := os.Stat(path)
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
	digest := contentHashFromBytes(hash.Sum(nil))
	if digests != nil && digests.reusable(path) && statErr == nil {
		digests.remember(path, before, digest)
	}
	return digest, true, nil
}

func hashPackageDirectory(ctx context.Context, directory string, overlays packageOverlays, listing *directoryListing) (identity.ContentHash, bool, error) {
	var types map[string]string
	if listing != nil {
		types = make(map[string]string, len(listing.types)+len(overlays.byPath))
		for name, typeStr := range listing.types {
			types[name] = typeStr
		}
	} else {
		entries, err := os.ReadDir(directory)
		if err != nil {
			if os.IsNotExist(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("read Go package directory %q: %w", directory, err)
		}
		types = make(map[string]string, len(entries)+len(overlays.byPath))
		for _, entry := range entries {
			types[entry.Name()] = entry.Type().String()
			if entry.IsDir() {
				types[entry.Name()] = "directory"
			}
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
