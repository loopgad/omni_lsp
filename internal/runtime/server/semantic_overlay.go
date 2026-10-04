package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const (
	goSemanticOverlayMaxOpenDocuments = 2048
	goSemanticOverlayMaxOpenBytes     = int64(32 << 20)
	goSemanticOverlayMaxMaterialized  = int64(128 << 20)
)

var (
	errGoSemanticOverlayUnavailable = errors.New("Go semantic overlay is unavailable")
	errGoSemanticOverlayStale       = errors.New("Go semantic overlay snapshot is stale")
	errGoSemanticOverlayIncomplete  = errors.New("Go semantic overlay is incomplete")
)

// goSnapshotSemanticView overlays the exact open Go documents from one LSP
// snapshot on an immutable disk view. It does not copy the whole workspace:
// reads use the disk capture or bounded editor buffers, and only a compiler
// materialization copies files (at most goSemanticOverlayMaxMaterialized bytes
// concurrently for this request view).
type goSnapshotSemanticView struct {
	base         *diskSemanticView
	diskExcluded string
	identity     model.Identity
	overlay      semanticOverlayIdentity
	snapshot     *snapshot.Snapshot
	overlayFiles map[string]goSemanticOverlayFile
	newFiles     []model.File
	changedLangs []string
	changedGo    bool
	mu           sync.Mutex
	materialized int64
	closed       bool
}

type goSemanticOverlayFile struct {
	file    model.File
	content []byte // immutable VFS bytes from the snapshot-bound revision
}

// captureGoSnapshotSemanticView binds a disk capture and open editor buffers
// to the request snapshot. The caller must have captured the disk view for the
// same revision. On success this view owns base and Close releases its private
// snapshot; on failure base remains caller-owned.
func captureGoSnapshotSemanticView(s *Server, ctx context.Context, base *diskSemanticView) (*goSnapshotSemanticView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil {
		return nil, fmt.Errorf("%w: request has no captured snapshot", errGoSemanticOverlayUnavailable)
	}
	requestIdentity, ok := captureSemanticOverlayIdentity(s, ctx, captured.ID().Revision)
	if !ok {
		return nil, fmt.Errorf("%w: VFS and snapshot documents differ", errGoSemanticOverlayUnavailable)
	}
	return captureGoSnapshotSemanticViewForIdentity(s, ctx, base, requestIdentity)
}

func captureGoSnapshotSemanticViewForIdentity(
	s *Server,
	ctx context.Context,
	base *diskSemanticView,
	requestIdentity semanticOverlayIdentity,
) (*goSnapshotSemanticView, error) {
	return captureGoSnapshotSemanticViewForIdentityExcluding(s, ctx, base, requestIdentity, "")
}

func captureGoSnapshotSemanticViewForIdentityExcluding(
	s *Server,
	ctx context.Context,
	base *diskSemanticView,
	requestIdentity semanticOverlayIdentity,
	diskExcluded string,
) (*goSnapshotSemanticView, error) {
	if s == nil || s.vfs == nil || base == nil || base.snapshot == "" {
		return nil, fmt.Errorf("%w: missing server, VFS, or disk view", errGoSemanticOverlayUnavailable)
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil || captured.ID().Revision == 0 || s.currentRevision() != captured.ID().Revision ||
		base.identity.Workspace == "" || base.identity.Workspace != identity.WorkspaceID(captured.ID().WorkspaceID) ||
		base.identity.SnapshotRev != captured.ID().Revision {
		return nil, fmt.Errorf("%w: disk view and request snapshot do not share a revision", errGoSemanticOverlayUnavailable)
	}
	if requestIdentity.Revision != captured.ID().Revision || requestIdentity.SnapshotInstance != captured.InstanceID() ||
		requestIdentity.Digest == "" || requestIdentity.VFSRevision != s.vfs.Revision() {
		return nil, errGoSemanticOverlayStale
	}
	openURIs := s.vfs.OpenFiles()
	sort.Strings(openURIs)
	if len(openURIs) > goSemanticOverlayMaxOpenDocuments {
		return nil, fmt.Errorf("%w: open document count exceeds %d", errGoSemanticOverlayUnavailable, goSemanticOverlayMaxOpenDocuments)
	}
	var openBytes int64
	for _, documentURI := range openURIs {
		state := s.vfs.Get(documentURI)
		if state == nil {
			return nil, fmt.Errorf("%w: open document changed during capture", errGoSemanticOverlayUnavailable)
		}
		if int64(len(state.Content)) > goSemanticOverlayMaxOpenBytes-openBytes {
			return nil, fmt.Errorf("%w: open document bytes exceed %d", errGoSemanticOverlayUnavailable, goSemanticOverlayMaxOpenBytes)
		}
		openBytes += int64(len(state.Content))
	}
	revision := captured.ID().Revision
	if requestIdentity.SnapshotInstance != captured.InstanceID() || s.vfs.Revision() != requestIdentity.VFSRevision {
		return nil, errGoSemanticOverlayStale
	}

	view := &goSnapshotSemanticView{
		base:         base,
		diskExcluded: diskExcluded,
		identity:     base.identity,
		overlay:      requestIdentity,
		snapshot:     captured,
		overlayFiles: make(map[string]goSemanticOverlayFile),
	}
	changedLanguages := make(map[string]struct{})
	seenURIs := make(map[string]struct{}, len(openURIs))
	for _, documentURI := range openURIs {
		state := s.vfs.Get(documentURI)
		if state == nil || s.vfs.Revision() != requestIdentity.VFSRevision {
			return nil, errGoSemanticOverlayStale
		}
		parsed, err := uri.Parse(state.URI)
		if err != nil || !parsed.IsFile() {
			return nil, fmt.Errorf("%w: open document is not a local file URI", errGoSemanticOverlayUnavailable)
		}
		canonicalURI := parsed.Canonical()
		if _, duplicate := seenURIs[canonicalURI]; duplicate {
			return nil, fmt.Errorf("%w: duplicate canonical open document URI", errGoSemanticOverlayUnavailable)
		}
		seenURIs[canonicalURI] = struct{}{}
		path, err := parsed.Path()
		if err != nil {
			return nil, fmt.Errorf("%w: resolve open document path: %v", errGoSemanticOverlayUnavailable, err)
		}
		path, err = filepath.Abs(path)
		if err != nil || ensureContained(base.rootPath, path) != nil || state.LanguageID == "" {
			return nil, fmt.Errorf("%w: open document is outside the workspace or has no language", errGoSemanticOverlayUnavailable)
		}
		rel, err := filepath.Rel(base.rootPath, path)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: invalid open document path", errGoSemanticOverlayUnavailable)
		}
		rel = filepath.ToSlash(rel)
		if semanticOverlayPathExcluded(base.rootPath, rel) {
			continue
		}
		content := state.Content // VFS buffers are immutable; the captured overlay identity verified these bytes.
		if int64(len(content)) > semanticViewMaxFileBytes {
			return nil, fmt.Errorf("%w: open document exceeds %d bytes", errGoSemanticOverlayUnavailable, semanticViewMaxFileBytes)
		}
		file := model.File{
			URI: canonicalURI, LanguageID: state.LanguageID,
			Size: int64(len(content)), SHA256: semanticOverlayContentHash(content),
		}
		if diskFile, exists := base.byURI[canonicalURI]; exists {
			changed := diskFile.LanguageID != file.LanguageID || diskFile.Size != file.Size || diskFile.SHA256 != file.SHA256
			if changed {
				if diskFile.LanguageID != "" {
					changedLanguages[diskFile.LanguageID] = struct{}{}
				}
				changedLanguages[file.LanguageID] = struct{}{}
				view.overlayFiles[canonicalURI] = goSemanticOverlayFile{file: file, content: content}
			}
			continue
		}
		changedLanguages[file.LanguageID] = struct{}{}
		view.overlayFiles[canonicalURI] = goSemanticOverlayFile{file: file, content: content}
		view.newFiles = append(view.newFiles, file)
	}
	if s.vfs.Revision() != requestIdentity.VFSRevision || !semanticOverlayStillCurrent(s, ctx, revision, requestIdentity) {
		return nil, errGoSemanticOverlayStale
	}
	view.identity.DiskDigest = semanticOverlayWorkspaceDigest(base.identity.DiskDigest, requestIdentity.Digest)
	view.identity.SnapshotRev = revision
	for language := range changedLanguages {
		view.changedLangs = append(view.changedLangs, language)
		if language == "go" {
			view.changedGo = true
		}
	}
	sort.Strings(view.changedLangs)
	sort.Slice(view.newFiles, func(i, j int) bool { return view.newFiles[i].URI < view.newFiles[j].URI })
	return view, nil
}

func semanticOverlayContentHash(content []byte) identity.ContentHash {
	sum := sha256.Sum256(content)
	return identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

func semanticOverlayWorkspaceDigest(disk, overlay identity.ContentHash) identity.ContentHash {
	sum := sha256.Sum256([]byte(string(disk) + "\x00" + string(overlay)))
	return identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

func semanticOverlayPathExcluded(root, relative string) bool {
	if skipSemanticGeneratedFile(relative) {
		return true
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(filepath.FromSlash(relative)), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if skipSemanticDirectory(root, current, part) {
			return true
		}
	}
	return false
}

func (v *goSnapshotSemanticView) Identity() model.Identity { return v.identity }

func (v *goSnapshotSemanticView) Close() error {
	if v == nil || v.base == nil {
		return nil
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	v.mu.Unlock()
	return v.base.Close()
}

func (v *goSnapshotSemanticView) ChangedLanguages() []string {
	return append([]string(nil), v.changedLangs...)
}

func (v *goSnapshotSemanticView) HasGoChanges() bool { return v != nil && v.changedGo }

func (v *goSnapshotSemanticView) SnapshotInstanceID() uint64 {
	if v == nil || v.snapshot == nil {
		return 0
	}
	return v.snapshot.InstanceID()
}

func (v *goSnapshotSemanticView) StillCurrent(s *Server, ctx context.Context) bool {
	return v != nil && semanticOverlayStillCurrent(s, ctx, v.identity.SnapshotRev, v.overlay)
}

func (v *goSnapshotSemanticView) DiskStillCurrent(ctx context.Context) bool {
	if v == nil || v.base == nil || ctx == nil || ctx.Err() != nil || v.base.identity.DiskDigest == "" {
		return false
	}
	digest, err := semanticDiskDigest(ctx, v.base.rootPath, v.diskExcluded)
	return err == nil && ctx.Err() == nil && digest == v.base.identity.DiskDigest
}

// exportGoSnapshotOverlay plans and exports all Go scopes from the exact
// request view. The supplied sink must be request-local staging; this helper
// never publishes an index generation. It rejects stale snapshots and reports
// whose symbol/declaration/definition/reference coverage is not complete.
func exportGoSnapshotOverlay(
	ctx context.Context,
	s *Server,
	view *goSnapshotSemanticView,
	builder languages.SemanticIndexRequestBuilder,
	provider languages.SemanticIndexProvider,
	sink model.Sink,
) (model.Request, model.Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || view == nil || builder == nil || provider == nil || sink == nil ||
		!view.HasGoChanges() || !view.changedLanguagesAreGo() {
		return model.Request{}, model.Report{}, errGoSemanticOverlayUnavailable
	}
	if !view.StillCurrent(s, ctx) {
		return model.Request{}, model.Report{}, errGoSemanticOverlayStale
	}
	request, err := builder.BuildIndexRequest(ctx, view, view.base.rootURI)
	if err != nil {
		return model.Request{}, model.Report{}, err
	}
	return exportGoSnapshotOverlayWithRequest(ctx, s, view, request, provider, sink)
}

func exportGoSnapshotOverlayWithRequest(
	ctx context.Context,
	s *Server,
	view *goSnapshotSemanticView,
	request model.Request,
	provider languages.SemanticIndexProvider,
	sink model.Sink,
) (model.Request, model.Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || view == nil || provider == nil || sink == nil || !view.HasGoChanges() || !view.changedLanguagesAreGo() {
		return model.Request{}, model.Report{}, errGoSemanticOverlayUnavailable
	}
	if !view.StillCurrent(s, ctx) {
		return model.Request{}, model.Report{}, errGoSemanticOverlayStale
	}
	if err := validateGoSnapshotOverlayPlan(ctx, view, request); err != nil {
		return model.Request{}, model.Report{}, err
	}
	report, err := provider.ExportIndex(ctx, request, sink)
	if err != nil {
		return model.Request{}, model.Report{}, err
	}
	if !view.StillCurrent(s, ctx) || !goSnapshotOverlayPlanToolsStillMatch(ctx, request) {
		return model.Request{}, model.Report{}, errGoSemanticOverlayStale
	}
	if err := model.ValidateReport(request, report); err != nil {
		return model.Request{}, model.Report{}, err
	}
	for _, scope := range request.Scopes {
		for _, fact := range [...]model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
		} {
			if !scopeFactComplete(report, scope.ID, fact) {
				return model.Request{}, model.Report{}, fmt.Errorf("%w: scope %q fact %q", errGoSemanticOverlayIncomplete, scope.ID, fact)
			}
		}
	}
	return request, report, nil
}

func validateGoSnapshotOverlayPlan(ctx context.Context, view *goSnapshotSemanticView, request model.Request) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if view == nil || view.base == nil || request.View == nil || request.View.Identity() != view.Identity() ||
		request.WorkspaceRootURI != view.base.rootURI || len(request.Scopes) == 0 ||
		len(request.Provenance) != len(request.Scopes) {
		return fmt.Errorf("%w: Go planner did not bind scopes and provenance to the request view", errGoSemanticOverlayUnavailable)
	}
	seen := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scope.Language != "go" || scope.ID == "" || scope.BuildContext == "" ||
			!semanticScopeWithinWorkspace(view.base.rootPath, scope.RootURI) {
			return fmt.Errorf("%w: invalid Go scope %q", errGoSemanticOverlayUnavailable, scope.ID)
		}
		if _, duplicate := seen[scope.ID]; duplicate {
			return fmt.Errorf("%w: duplicate Go scope %q", errGoSemanticOverlayIncomplete, scope.ID)
		}
		seen[scope.ID] = struct{}{}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != view.Identity() ||
			!reflect.DeepEqual(provenance.Scope, scope) || provenance.Extractor == "" || provenance.ExtractorVer == "" ||
			provenance.Toolchain == "" || provenance.Backend.Name == "" || provenance.Backend.Language != "go" ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return fmt.Errorf("%w: invalid provenance for Go scope %q", errGoSemanticOverlayUnavailable, scope.ID)
		}
		if len(provenance.Tools) == 0 {
			return fmt.Errorf("%w: Go scope %q has no pinned tool", errGoSemanticOverlayUnavailable, scope.ID)
		}
		for _, tool := range provenance.Tools {
			if tool.Name == "" || tool.Path == "" || tool.Version == "" || tool.SHA256 == "" ||
				!toolIdentityStillMatches(ctx, tool) {
				return fmt.Errorf("%w: pinned tool for Go scope %q changed or is unavailable", errGoSemanticOverlayStale, scope.ID)
			}
		}
	}
	return nil
}

func (v *goSnapshotSemanticView) changedLanguagesAreGo() bool {
	if v == nil || !v.changedGo || len(v.changedLangs) == 0 {
		return false
	}
	for _, language := range v.changedLangs {
		if language != "go" {
			return false
		}
	}
	return true
}

func scopeFactComplete(report model.Report, scopeID string, fact model.FactKind) bool {
	for _, coverage := range report.Coverage {
		if coverage.ScopeID == scopeID && coverage.Fact == fact {
			return coverage.State == model.Complete
		}
	}
	return false
}

func (v *goSnapshotSemanticView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	if v == nil || v.base == nil || v.closed {
		return errors.New("semantic index: request semantic view is closed")
	}
	if visit == nil {
		return errors.New("semantic index: nil file visitor")
	}
	parsed, err := uri.Parse(rootURI)
	if err != nil || !parsed.IsFile() {
		return fmt.Errorf("semantic index: invalid scope root URI %q", rootURI)
	}
	rootPath, err := parsed.Path()
	if err != nil {
		return err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return err
	}
	if err := ensureContained(v.base.rootPath, rootPath); err != nil {
		return fmt.Errorf("semantic index: scope root is outside captured workspace: %w", err)
	}

	baseIndex, newIndex := 0, 0
	var baseFile, newFile model.File
	var hasBase, hasNew bool
	loadBase := func() error {
		if hasBase {
			return nil
		}
		for baseIndex < len(v.base.files) {
			file := v.base.files[baseIndex]
			baseIndex++
			if overlay, ok := v.overlayFiles[file.URI]; ok {
				file = overlay.file
			}
			inside, pathErr := semanticFileWithin(rootPath, file.URI)
			if pathErr != nil {
				return pathErr
			}
			if inside {
				baseFile, hasBase = file, true
				return nil
			}
		}
		return nil
	}
	loadNew := func() error {
		if hasNew {
			return nil
		}
		for newIndex < len(v.newFiles) {
			candidate := v.newFiles[newIndex]
			newIndex++
			inside, pathErr := semanticFileWithin(rootPath, candidate.URI)
			if pathErr != nil {
				return pathErr
			}
			if inside {
				newFile, hasNew = candidate, true
				return nil
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := loadBase(); err != nil {
			return err
		}
		if err := loadNew(); err != nil {
			return err
		}
		if !hasBase && !hasNew {
			return nil
		}
		if hasBase && (!hasNew || baseFile.URI < newFile.URI) {
			if err := visit(baseFile); err != nil {
				return err
			}
			hasBase = false
			continue
		}
		if hasNew && (!hasBase || newFile.URI < baseFile.URI) {
			if err := visit(newFile); err != nil {
				return err
			}
			hasNew = false
			continue
		}
		return fmt.Errorf("semantic index: duplicate request snapshot file URI %q", newFile.URI)
	}
}

func semanticFileWithin(rootPath, fileURI string) (bool, error) {
	parsed, err := uri.Parse(fileURI)
	if err != nil || !parsed.IsFile() {
		return false, fmt.Errorf("semantic index: invalid captured file URI %q", fileURI)
	}
	path, err := parsed.Path()
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootPath, path)
	if err != nil {
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}

func (v *goSnapshotSemanticView) Read(ctx context.Context, fileURI string) (io.ReadCloser, error) {
	if v == nil || v.base == nil || v.closed {
		return nil, errors.New("semantic index: request semantic view is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if overlay, ok := v.overlayFiles[fileURI]; ok {
		return io.NopCloser(bytes.NewReader(overlay.content)), nil
	}
	return v.base.Read(ctx, fileURI)
}

func (v *goSnapshotSemanticView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if v == nil || v.base == nil || v.closed {
		return nil, errors.New("semantic index: request semantic view is closed")
	}
	parsed, err := uri.Parse(rootURI)
	if err != nil || !parsed.IsFile() {
		return nil, fmt.Errorf("semantic index: invalid materialization root %q", rootURI)
	}
	logicalRootPath, err := parsed.Path()
	if err != nil {
		return nil, err
	}
	logicalRootPath, err = filepath.Abs(logicalRootPath)
	if err != nil {
		return nil, err
	}
	if err := ensureContained(v.base.rootPath, logicalRootPath); err != nil {
		return nil, fmt.Errorf("semantic index: materialization root is outside captured workspace: %w", err)
	}
	if destination == "" {
		return nil, errors.New("semantic index: request materializer requires an owned destination")
	}
	if err := ensureOutsideWorkspace(v.base.rootPath, destination); err != nil {
		return nil, fmt.Errorf("semantic index: materialized destination must be outside workspace: %w", err)
	}
	scopeRel, err := filepath.Rel(v.base.rootPath, logicalRootPath)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(destination, scopeRel)
	if err := ensureContained(destination, root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	var bytesNeeded int64
	if err := v.Walk(ctx, rootURI, func(file model.File) error {
		if file.Size < 0 || file.Size > semanticViewMaxFileBytes || file.Size > goSemanticOverlayMaxMaterialized-bytesNeeded {
			return fmt.Errorf("semantic index: Go request materialization exceeds %d bytes", goSemanticOverlayMaxMaterialized)
		}
		bytesNeeded += file.Size
		return nil
	}); err != nil {
		_ = removeReadOnlyTree(root)
		return nil, err
	}
	if !v.reserveMaterialization(bytesNeeded) {
		_ = removeReadOnlyTree(root)
		return nil, fmt.Errorf("semantic index: Go request materialization budget %d bytes is exhausted", goSemanticOverlayMaxMaterialized)
	}
	reserved := true
	cleanup := func() {
		_ = removeReadOnlyTree(root)
		if reserved {
			v.releaseMaterialization(bytesNeeded)
			reserved = false
		}
	}
	allowedPaths := map[string]struct{}{".": {}}
	copyErr := v.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parsedFile, err := uri.Parse(file.URI)
		if err != nil {
			return err
		}
		filePath, err := parsedFile.Path()
		if err != nil {
			return err
		}
		logicalRel, err := filepath.Rel(logicalRootPath, filePath)
		if err != nil || logicalRel == ".." || strings.HasPrefix(logicalRel, ".."+string(filepath.Separator)) || filepath.IsAbs(logicalRel) {
			return fmt.Errorf("semantic index: file %q escaped its Go scope", file.URI)
		}
		dest := filepath.Join(root, logicalRel)
		if err := ensureContained(root, dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		reader, err := v.Read(ctx, file.URI)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = reader.Close()
			return err
		}
		hash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(contextReader{ctx: ctx, reader: reader}, file.Size+1))
		closeErr := errors.Join(reader.Close(), out.Close())
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if n != file.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != string(file.SHA256) {
			return fmt.Errorf("semantic index: materialized request file %q changed", file.URI)
		}
		if err := os.Chmod(dest, 0o400); err != nil {
			return err
		}
		allowedPaths[filepath.Clean(logicalRel)] = struct{}{}
		for dir := filepath.Dir(logicalRel); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			allowedPaths[filepath.Clean(dir)] = struct{}{}
		}
		return nil
	})
	if copyErr != nil {
		cleanup()
		return nil, copyErr
	}
	if err := makeReadOnlyTree(root); err != nil {
		cleanup()
		return nil, err
	}
	materialized := &goOverlayMaterializedView{
		semanticMaterializedView: &semanticMaterializedView{
			rootURI: parsed.Canonical(), logicalRootPath: logicalRootPath, rootPath: root, allowedPaths: allowedPaths,
		},
		owner: v, cleanupRoot: root, reserved: bytesNeeded,
	}
	reserved = false
	return materialized, nil
}

func (v *goSnapshotSemanticView) reserveMaterialization(size int64) bool {
	if size < 0 || size > goSemanticOverlayMaxMaterialized {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.materialized > goSemanticOverlayMaxMaterialized-size {
		return false
	}
	v.materialized += size
	return true
}

func (v *goSnapshotSemanticView) releaseMaterialization(size int64) {
	v.mu.Lock()
	if size > v.materialized {
		v.materialized = 0
	} else {
		v.materialized -= size
	}
	v.mu.Unlock()
}

type goOverlayMaterializedView struct {
	*semanticMaterializedView
	owner       *goSnapshotSemanticView
	cleanupRoot string
	reserved    int64
	closeMu     sync.Mutex
	closed      bool
}

func (v *goOverlayMaterializedView) Close() error {
	v.closeMu.Lock()
	if v.closed {
		v.closeMu.Unlock()
		return nil
	}
	v.closed = true
	v.semanticMaterializedView.closed = true
	v.closeMu.Unlock()
	err := removeReadOnlyTree(v.cleanupRoot)
	v.owner.releaseMaterialization(v.reserved)
	return err
}

var _ model.WorkspaceView = (*goSnapshotSemanticView)(nil)
var _ model.WorkspaceMaterializer = (*goSnapshotSemanticView)(nil)
var _ model.MaterializedView = (*goOverlayMaterializedView)(nil)
