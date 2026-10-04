package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/fileio"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const (
	semanticViewMaxFileBytes = int64(256 << 20)
	semanticViewMaxBytes     = int64(8 << 30)
	semanticViewMaxDepth     = 64
)

var errSemanticViewIncomplete = errors.New("semantic index: workspace view is incomplete")

type semanticFileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// diskSemanticView captures a path/hash manifest. Reads and materialization
// verify the captured hash, so a concurrent edit becomes an error instead of
// leaking mutable workspace bytes into an extraction run.
type diskSemanticView struct {
	rootPath string
	rootURI  string
	snapshot string
	identity model.Identity
	files    []model.File
	byURI    map[string]model.File
	byPath   map[string]model.File
}

func captureSemanticView(ctx context.Context, rootPath string, workspace identity.WorkspaceID, revision uint64, excludedDir string) (*diskSemanticView, error) {
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}
	rootURI := uri.FromPath(rootPath).Canonical()
	canonicalWorkspace := identity.WorkspaceID(rootURI)
	if workspace != "" && workspace != canonicalWorkspace {
		return nil, fmt.Errorf("semantic index: workspace identity does not match canonical root URI")
	}
	workspace = canonicalWorkspace
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("semantic index: workspace root unavailable: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("semantic index: workspace root is not a directory: %s", rootPath)
	}
	snapshot, err := os.MkdirTemp("", "omnilsp-captured-view-")
	if err != nil {
		return nil, err
	}
	keepSnapshot := false
	defer func() {
		if !keepSnapshot {
			_ = removeReadOnlyTree(snapshot)
		}
	}()
	if err := ensureOutsideWorkspace(rootPath, snapshot); err != nil {
		return nil, err
	}
	var excludedInfo fs.FileInfo
	if excludedDir != "" {
		excludedInfo, _ = os.Stat(excludedDir)
	}
	records := make([]semanticFileRecord, 0, 1024)
	files := make([]model.File, 0, 1024)
	byURI := make(map[string]model.File)
	byPath := make(map[string]model.File)
	var total int64
	err = filepath.WalkDir(rootPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == rootPath {
			return nil
		}
		if entry.IsDir() {
			if excludedInfo != nil {
				info, infoErr := entry.Info()
				if infoErr != nil {
					return infoErr
				}
				if os.SameFile(info, excludedInfo) {
					return filepath.SkipDir
				}
			}
			if skipSemanticDirectory(rootPath, path, entry.Name()) {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(rootPath, path)
			if relErr != nil {
				return relErr
			}
			if strings.Count(filepath.ToSlash(rel), "/") >= semanticViewMaxDepth {
				return fmt.Errorf("%w: directory depth exceeds %d at %s", errSemanticViewIncomplete, semanticViewMaxDepth, rel)
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink requires an explicit immutable source mapping: %s", errSemanticViewIncomplete, path)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: non-regular workspace entry: %s", errSemanticViewIncomplete, path)
		}
		relPath, relErr := filepath.Rel(rootPath, path)
		if relErr != nil {
			return relErr
		}
		if skipSemanticGeneratedFile(relPath) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() < 0 || info.Size() > semanticViewMaxFileBytes {
			return fmt.Errorf("%w: file size %d exceeds supported limit at %s", errSemanticViewIncomplete, info.Size(), path)
		}
		if total > semanticViewMaxBytes-info.Size() {
			return fmt.Errorf("%w: workspace exceeds %d bytes", errSemanticViewIncomplete, semanticViewMaxBytes)
		}
		rel, relErr := filepath.Rel(rootPath, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		f, openErr := fileio.OpenReadShared(path)
		if openErr != nil {
			return openErr
		}
		snapshotPath := filepath.Join(snapshot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
			_ = f.Close()
			return err
		}
		captured, openErr := os.OpenFile(snapshotPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr != nil {
			_ = f.Close()
			return openErr
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(h, captured), io.LimitReader(f, semanticViewMaxFileBytes+1))
		closeErr := errors.Join(f.Close(), captured.Close())
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != info.Size() || n > semanticViewMaxFileBytes {
			return fmt.Errorf("semantic index: workspace changed while capturing %s", rel)
		}
		if err := os.Chmod(snapshotPath, 0o400); err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		fileURI := uri.FromPath(path).Canonical()
		file := model.File{URI: fileURI, LanguageID: semanticLanguageID(path), Size: n, SHA256: identity.ContentHash("sha256:" + sum)}
		records = append(records, semanticFileRecord{Path: rel, Size: n, SHA256: sum})
		files = append(files, file)
		byURI[fileURI], byPath[rel] = file, file
		total += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := makeReadOnlyTree(snapshot); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	sort.Slice(files, func(i, j int) bool { return files[i].URI < files[j].URI })
	digest, err := semanticRecordsDigest(records)
	if err != nil {
		return nil, err
	}
	view := &diskSemanticView{
		rootPath: rootPath,
		rootURI:  rootURI,
		snapshot: snapshot,
		identity: model.Identity{Workspace: workspace, DiskDigest: digest, SnapshotRev: revision},
		files:    files, byURI: byURI, byPath: byPath,
	}
	keepSnapshot = true
	return view, nil
}

func semanticRecordsDigest(records []semanticFileRecord) (identity.ContentHash, error) {
	payload, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return identity.ContentHash("sha256:" + hex.EncodeToString(digest[:])), nil
}

// semanticDiskDigest rechecks the complete source tree without creating a
// second snapshot. It intentionally uses the same fail-closed file rules and
// record encoding as captureSemanticView.
func semanticDiskDigest(ctx context.Context, rootPath string, excludedDir string) (identity.ContentHash, error) {
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("semantic index: workspace root is not a directory: %s", rootPath)
	}
	var excludedInfo fs.FileInfo
	if excludedDir != "" {
		excludedInfo, _ = os.Stat(excludedDir)
	}
	records := make([]semanticFileRecord, 0, 1024)
	var total int64
	err = filepath.WalkDir(rootPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == rootPath {
			return nil
		}
		if entry.IsDir() {
			if excludedInfo != nil {
				info, infoErr := entry.Info()
				if infoErr != nil {
					return infoErr
				}
				if os.SameFile(info, excludedInfo) {
					return filepath.SkipDir
				}
			}
			if skipSemanticDirectory(rootPath, path, entry.Name()) {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(rootPath, path)
			if relErr != nil {
				return relErr
			}
			if strings.Count(filepath.ToSlash(rel), "/") >= semanticViewMaxDepth {
				return fmt.Errorf("%w: directory depth exceeds %d at %s", errSemanticViewIncomplete, semanticViewMaxDepth, rel)
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink requires an explicit immutable source mapping: %s", errSemanticViewIncomplete, path)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: non-regular workspace entry: %s", errSemanticViewIncomplete, path)
		}
		relPath, relErr := filepath.Rel(rootPath, path)
		if relErr != nil {
			return relErr
		}
		if skipSemanticGeneratedFile(relPath) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > semanticViewMaxFileBytes {
			return fmt.Errorf("%w: file size %d exceeds supported limit at %s", errSemanticViewIncomplete, info.Size(), path)
		}
		if total > semanticViewMaxBytes-info.Size() {
			return fmt.Errorf("%w: workspace exceeds %d bytes", errSemanticViewIncomplete, semanticViewMaxBytes)
		}
		rel, err := filepath.Rel(rootPath, path)
		if err != nil {
			return err
		}
		f, err := fileio.OpenReadShared(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, copyErr := io.Copy(h, io.LimitReader(f, semanticViewMaxFileBytes+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != info.Size() || n > semanticViewMaxFileBytes {
			return fmt.Errorf("semantic index: workspace changed while verifying %s", rel)
		}
		records = append(records, semanticFileRecord{Path: filepath.ToSlash(rel), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		total += n
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return semanticRecordsDigest(records)
}

func (v *diskSemanticView) Identity() model.Identity { return v.identity }

// Close removes the private immutable content snapshot created at capture.
func (v *diskSemanticView) Close() error {
	if v.snapshot == "" {
		return nil
	}
	snapshot := v.snapshot
	v.snapshot = ""
	return removeReadOnlyTree(snapshot)
}

func (v *diskSemanticView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	if visit == nil {
		return errors.New("semantic index: nil file visitor")
	}
	root, err := uri.Parse(rootURI)
	if err != nil || !root.IsFile() {
		return fmt.Errorf("semantic index: invalid scope root URI %q", rootURI)
	}
	rootPath, err := root.Path()
	if err != nil {
		return err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return err
	}
	if err := ensureContained(v.rootPath, rootPath); err != nil {
		return fmt.Errorf("semantic index: scope root is outside captured workspace: %w", err)
	}
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		fileURI, err := uri.Parse(file.URI)
		if err != nil {
			return err
		}
		filePath, err := fileURI.Path()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(rootPath, filePath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *diskSemanticView) Read(ctx context.Context, fileURI string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v.snapshot == "" {
		return nil, os.ErrClosed
	}
	file, ok := v.byURI[fileURI]
	if !ok {
		return nil, os.ErrNotExist
	}
	parsed, err := uri.Parse(file.URI)
	if err != nil {
		return nil, err
	}
	path, err := parsed.Path()
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(v.rootPath, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(v.snapshot, rel))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != file.Size || "sha256:"+hex.EncodeToString(sum[:]) != string(file.SHA256) {
		return nil, fmt.Errorf("semantic index: captured file changed: %s", fileURI)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (v *diskSemanticView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
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
	if err := ensureContained(v.rootPath, logicalRootPath); err != nil {
		return nil, fmt.Errorf("semantic index: materialization root is outside captured workspace: %w", err)
	}
	if v.snapshot == "" {
		return nil, os.ErrClosed
	}
	if destination != "" {
		if err := ensureOutsideWorkspace(v.rootPath, destination); err != nil {
			return nil, fmt.Errorf("semantic index: materialized destination must be outside workspace: %w", err)
		}
	}
	allowedPaths := map[string]struct{}{".": {}}
	paths := make([]string, 0, len(v.byPath))
	for rel := range v.byPath {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := filepath.Join(v.rootPath, filepath.FromSlash(rel))
		logicalRel, relErr := filepath.Rel(logicalRootPath, source)
		if relErr != nil || logicalRel == ".." || strings.HasPrefix(logicalRel, ".."+string(filepath.Separator)) || filepath.IsAbs(logicalRel) {
			continue
		}
		dest := filepath.Join(v.snapshot, filepath.FromSlash(rel))
		if err := ensureContained(v.snapshot, dest); err != nil {
			return nil, err
		}
		if _, err := os.Stat(dest); err != nil {
			return nil, err
		}
		allowedPaths[filepath.Clean(logicalRel)] = struct{}{}
		for dir := filepath.Dir(logicalRel); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			allowedPaths[filepath.Clean(dir)] = struct{}{}
		}
	}
	scopeRel, err := filepath.Rel(v.rootPath, logicalRootPath)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(v.snapshot, scopeRel)
	if err := ensureContained(v.snapshot, root); err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	return &semanticMaterializedView{rootURI: parsed.Canonical(), logicalRootPath: logicalRootPath, rootPath: root, allowedPaths: allowedPaths}, nil
}

type semanticMaterializedView struct {
	rootURI         string
	logicalRootPath string
	rootPath        string
	allowedPaths    map[string]struct{}
	closed          bool
}

func (v *semanticMaterializedView) RootURI() string  { return v.rootURI }
func (v *semanticMaterializedView) RootPath() string { return v.rootPath }
func (v *semanticMaterializedView) PathForURI(value string) (string, error) {
	if v.closed {
		return "", os.ErrClosed
	}
	parsed, err := uri.Parse(value)
	if err != nil || !parsed.IsFile() {
		return "", fmt.Errorf("semantic index: invalid logical source URI %q", value)
	}
	filePath, err := parsed.Path()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(v.logicalRootPath, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("semantic index: URI outside materialized workspace: %q", value)
	}
	if _, ok := v.allowedPaths[filepath.Clean(rel)]; !ok {
		return "", fmt.Errorf("semantic index: URI is absent from captured workspace view: %q", value)
	}
	result := filepath.Join(v.rootPath, rel)
	if err := ensureContained(v.rootPath, result); err != nil {
		return "", err
	}
	return result, nil
}
func (v *semanticMaterializedView) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	return nil
}

func ensureOutsideWorkspace(workspace, target string) error {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(workspace, target)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return fmt.Errorf("path is inside workspace")
	}
	return nil
}

func ensureContained(root, target string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("semantic index: path escapes captured workspace")
	}
	return nil
}

func makeReadOnlyTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o500)
		if !entry.IsDir() {
			mode = 0o400
		}
		return os.Chmod(path, mode)
	})
}

func removeReadOnlyTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			if entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			} else {
				_ = os.Chmod(path, 0o600)
			}
		}
		return nil
	})
	return os.RemoveAll(root)
}

func skipSemanticVCSDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn":
		return true
	default:
		return false
	}
}

// skipSemanticDirectory excludes only VCS metadata and repository-local tool
// outputs created by this project. Language dependencies such as node_modules
// remain part of the captured view because project resolution may read them.
func skipSemanticDirectory(root, path, name string) bool {
	if skipSemanticVCSDir(name) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	switch filepath.ToSlash(rel) {
	case ".tmp-gocache", "dist", "dist-smoke", "test/acceptance/evidence",
		"test/acceptance/tools/bin", "test/acceptance/tools/node_modules", "test/acceptance/tools/.npm-cache",
		"editors/vscode/node_modules":
		return true
	default:
		return false
	}
}

func skipSemanticGeneratedFile(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".exe", ".test", ".dll", ".pdb", ".obj", ".o", ".a", ".so", ".dylib":
		return true
	default:
		return false
	}
}

func semanticLanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp", ".inl":
		return "cpp"
	case ".rs":
		return "rust"
	case ".py", ".pyi":
		return "python"
	case ".ts", ".mts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	default:
		return ""
	}
}

var _ model.WorkspaceView = (*diskSemanticView)(nil)
var _ model.WorkspaceMaterializer = (*diskSemanticView)(nil)
var _ model.MaterializedView = (*semanticMaterializedView)(nil)
