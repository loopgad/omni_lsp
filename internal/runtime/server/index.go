package server

import (
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
	"sync"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// v2 binds persisted freshness to the canonical workspace root and disk tree;
// process-local snapshot revisions remain build guards only.
const inventoryVersion = "file-inventory-v2"
const maxIndexDepth = 16
const maxIndexFileSize = 1 << 20

type fileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// IndexRebuildStats describes the last attempted rebuild, including failure.
type IndexRebuildStats struct {
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
	Skipped    int    `json:"skipped"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error"`
}

type semanticCoverageStat struct {
	ScopeID string             `json:"scopeId"`
	Fact    model.FactKind     `json:"fact"`
	State   model.Completeness `json:"state"`
	Reason  string             `json:"reason,omitempty"`
}

// IndexStats is the C12 read-only status projection.
type IndexStats struct {
	Enabled          bool                    `json:"enabled"`
	Reason           string                  `json:"reason,omitempty"`
	IndexDir         string                  `json:"indexDir,omitempty"`
	Generation       uint64                  `json:"generation"`
	Segments         []persistent.SegmentRef `json:"segments"`
	Quarantined      []persistent.SegmentID  `json:"quarantined"`
	DiskBudgetBytes  int64                   `json:"diskBudgetBytes"`
	LastRebuild      IndexRebuildStats       `json:"lastRebuild"`
	Fresh            bool                    `json:"fresh"`
	Revision         uint64                  `json:"revision"`
	SemanticStatus   string                  `json:"semanticStatus,omitempty"`
	SemanticCoverage []semanticCoverageStat  `json:"semanticCoverage,omitempty"`
}

type indexService struct {
	root        string
	workspaceID identity.WorkspaceID
	dir         string
	store       *persistent.FileStore
	budget      int64
	buildMu     sync.Mutex
	statsMu     sync.RWMutex
	last        IndexRebuildStats
}

func projectSemanticCoverage(coverage []model.Coverage) []semanticCoverageStat {
	out := make([]semanticCoverageStat, len(coverage))
	for i, item := range coverage {
		out[i] = semanticCoverageStat{ScopeID: item.ScopeID, Fact: item.Fact, State: item.State, Reason: item.Reason}
	}
	return out
}

func defaultIndexDir(root string) (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	h := sha256.Sum256([]byte(root))
	return filepath.Join(base, "omnilsp", "index", hex.EncodeToString(h[:8])), nil
}

// initIndex degrades on any local index or trust configuration error: initialize
// always completes and indexStats reports the reason.
func (s *Server) initIndex(rootURI string) {
	u, err := uri.Parse(rootURI)
	var root string
	if err == nil {
		root, err = u.Path()
	}
	if err == nil {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		s.disableIndex(err)
		return
	}
	workspaceID := identity.WorkspaceID(uri.FromPath(root).Canonical())
	policy, err := trust.PolicyForBackend(root, "server")
	if err != nil {
		s.disableIndex(err)
		return
	}
	dir := s.config.IndexDir
	if dir == "" {
		dir, err = defaultIndexDir(root)
	}
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		s.disableIndex(err)
		return
	}
	store, err := persistent.NewFileStore(dir, persistent.Config{DiskBudgetBytes: s.config.IndexDiskBudgetBytes})
	if err != nil {
		s.disableIndex(err)
		return
	}
	s.mu.Lock()
	s.idx = &indexService{root: root, workspaceID: workspaceID, dir: dir, store: store, budget: s.config.IndexDiskBudgetBytes}
	s.idxReason = ""
	s.trust = policy
	s.mu.Unlock()
}

// InitializeWorkspace configures workspace identity and persistent-index state
// for transports without an LSP initialize request, such as MCP.
func (s *Server) InitializeWorkspace(root string) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		s.disableIndex(err)
		return
	}
	rootURI := uri.FromPath(absolute).Canonical()
	s.mu.Lock()
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.mu.Unlock()
	s.initIndex(rootURI)
}

func (s *Server) disableIndex(err error) {
	s.mu.Lock()
	s.idx = nil
	s.idxReason = err.Error()
	s.mu.Unlock()
	fmt.Fprintf(os.Stderr, "omnilsp: persistent index disabled: %v\n", err)
}

func (s *Server) indexState() (*indexService, *trust.Policy, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx, s.trust, s.idxReason
}

func (s *Server) handleIndexStats(ctx context.Context, _ *jsonrpc.Message) (json.RawMessage, error) {
	idx, _, reason := s.indexState()
	stats := IndexStats{Segments: []persistent.SegmentRef{}, Quarantined: []persistent.SegmentID{}, Revision: s.currentRevision()}
	if idx == nil {
		if reason == "" {
			reason = "index not initialized"
		}
		stats.Reason = reason
		return json.Marshal(stats)
	}
	stats.Enabled, stats.IndexDir, stats.DiskBudgetBytes = true, idx.dir, idx.budget
	if len(s.semanticIndexBindings()) > 0 {
		stats.SemanticStatus = "not_built"
	} else {
		stats.SemanticStatus = "unverified"
	}
	idx.statsMu.RLock()
	stats.LastRebuild = idx.last
	idx.statsMu.RUnlock()
	lease, err := idx.store.OpenSnapshotLease(ctx)
	stats.Quarantined = idx.store.Quarantined()
	if errors.Is(err, persistent.ErrNoGeneration) {
		return json.Marshal(stats)
	}
	if err != nil {
		stats.Reason = err.Error()
		return json.Marshal(stats)
	}
	defer lease.Close()
	view := lease.Snapshot()
	stats.Generation, stats.Segments = view.ID, view.Segments
	semanticReader, semanticErr := semantic.OpenReader(ctx, view)
	if semanticErr == nil {
		defer semanticReader.Close()
		metadata := semanticReader.Metadata()
		stats.SemanticCoverage = projectSemanticCoverage(metadata.Coverage)
		stats.SemanticStatus = "disk_stale"
		if metadata.Identity.Workspace != idx.workspaceID {
			stats.Reason = "semantic generation belongs to a different workspace"
			return json.Marshal(stats)
		}
		currentDigest, digestErr := semanticDiskDigest(ctx, idx.root, idx.dir)
		if digestErr != nil {
			stats.Reason = digestErr.Error()
			return json.Marshal(stats)
		}
		stats.Fresh = metadata.DiskDigest == currentDigest
		if stats.Fresh {
			stats.SemanticStatus = "disk_fresh"
		}
		return json.Marshal(stats)
	}
	if !errors.Is(semanticErr, semantic.ErrNotSemantic) {
		stats.SemanticStatus = "invalid"
		stats.Reason = semanticErr.Error()
		return json.Marshal(stats)
	}
	stats.SemanticStatus = "legacy_inventory"
	if len(view.Segments) == 1 {
		sealed, readErr := view.ReadSegment(view.Segments[0].ID)
		var currentHash string
		if readErr == nil {
			currentFiles, inventoryErr := idx.inventory(ctx, &IndexRebuildStats{})
			if inventoryErr != nil {
				readErr = inventoryErr
			} else {
				var inventoryHash string
				_, inventoryHash, readErr = inventoryPayload(currentFiles)
				if readErr == nil {
					currentHash = inventorySourceHash(idx.workspaceID, inventoryHash)
				}
			}
		}
		if readErr == nil {
			_, readErr = persistent.VerifyPayload(sealed, persistent.FreshnessTuple{
				SourceHash: currentHash, BackendVer: inventoryVersion,
			})
		}
		stats.Fresh = readErr == nil
		if readErr != nil && !errors.Is(readErr, persistent.ErrStaleFreshness) {
			stats.Reason = readErr.Error()
		}
	}
	return json.Marshal(stats)
}

func (s *Server) handleReindex(ctx context.Context, _ *jsonrpc.Message) (json.RawMessage, error) {
	idx, policy, reason := s.indexState()
	if idx == nil {
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "index disabled: " + reason}
	}
	if policy == nil || policy.State() == trust.StateUntrusted {
		gateErr := ierrors.New(ierrors.ErrUntrustedOperation, "reindex", "workspace is untrusted")
		return nil, &jsonrpc.ResponseError{
			Code: jsonrpc.RequestFailed, Message: fmt.Sprintf("reindex rejected: %v", gateErr),
			Data: json.RawMessage(`{"kind":"untrusted_operation"}`),
		}
	}
	if !idx.buildMu.TryLock() {
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "reindex already running"}
	}
	defer idx.buildMu.Unlock()
	// Serialize rebuilds across processes that share the same index directory.
	// This lease is independent of the storage writer lock; extraction continues
	// without blocking readers or short-lived segment publication operations.
	reindexLease, err := idx.store.AcquireReindexLease(ctx)
	if err != nil {
		return nil, reindexResponseError(err)
	}
	defer reindexLease.Close()
	started := time.Now()
	revision := s.currentRevision()
	last := IndexRebuildStats{}
	defer func() {
		last.DurationMs = time.Since(started).Milliseconds()
		idx.statsMu.Lock()
		idx.last = last
		idx.statsMu.Unlock()
	}()
	if len(s.semanticIndexBindings()) > 0 {
		result, semanticErr := s.reindexSemantic(ctx, idx, revision, &last)
		if semanticErr != nil {
			last.Error = semanticErr.Error()
			return nil, reindexResponseError(semanticErr)
		}
		return result, nil
	}
	records, err := idx.inventory(ctx, &last)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		last.Error = err.Error()
		return nil, reindexResponseError(err)
	}
	data, inventoryHash, err := inventoryPayload(records)
	if err != nil {
		last.Error = err.Error()
		return nil, reindexResponseError(err)
	}
	sourceHash := inventorySourceHash(idx.workspaceID, inventoryHash)
	build, err := idx.store.BeginBuild(ctx)
	if err != nil {
		last.Error = err.Error()
		return nil, reindexResponseError(err)
	}
	generation := build.GenerationID()
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	_, err = build.WriteSegment(persistent.SealPayload(data, persistent.FreshnessTuple{
		SourceHash: sourceHash, BackendVer: inventoryVersion,
	}))
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		currentFiles, inventoryErr := idx.inventory(ctx, &IndexRebuildStats{})
		if inventoryErr != nil {
			err = inventoryErr
		} else {
			_, currentHash, hashErr := inventoryPayload(currentFiles)
			if hashErr != nil {
				err = hashErr
			} else if currentHash != inventoryHash {
				err = fmt.Errorf("workspace files changed during reindex: %s", firstInventoryChange(records, currentFiles))
			}
		}
	}
	if err == nil && s.currentRevision() != revision {
		err = fmt.Errorf("workspace changed during reindex (snapshot %d, current %d)", revision, s.currentRevision())
	}
	if err == nil {
		err = build.Commit(ctx)
	}
	if err != nil {
		last.Error = err.Error()
		return nil, reindexResponseError(err)
	}
	committed = true
	return json.Marshal(map[string]any{"generation": generation, "files": last.Files, "bytes": last.Bytes, "skipped": last.Skipped, "revision": revision})
}

func firstInventoryChange(before, after []fileRecord) string {
	for i, j := 0, 0; i < len(before) || j < len(after); {
		if i == len(before) || (j < len(after) && after[j].Path < before[i].Path) {
			return "added " + after[j].Path
		}
		if j == len(after) || before[i].Path < after[j].Path {
			return "removed " + before[i].Path
		}
		if before[i].Size != after[j].Size || before[i].SHA256 != after[j].SHA256 {
			return "changed " + before[i].Path
		}
		i++
		j++
	}
	return "inventory digest changed without a differing file record"
}

func reindexResponseError(err error) *jsonrpc.ResponseError {
	code := jsonrpc.RequestFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = jsonrpc.RequestCancelled
	}
	return &jsonrpc.ResponseError{Code: code, Message: err.Error()}
}

func inventoryPayload(records []fileRecord) ([]byte, string, error) {
	data, err := json.Marshal(records)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(data)
	return data, hex.EncodeToString(hash[:]), nil
}

func inventorySourceHash(workspaceID identity.WorkspaceID, inventoryHash string) string {
	key := "omnilsp-file-inventory\x00" + string(workspaceID) + "\x00" + inventoryVersion + "\x00" + inventoryHash
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func (idx *indexService) inventory(ctx context.Context, stats *IndexRebuildStats) ([]fileRecord, error) {
	records := make([]fileRecord, 0)
	indexInfo, err := os.Stat(idx.dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(idx.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if os.SameFile(info, indexInfo) {
				return filepath.SkipDir
			}
		}
		rel, err := filepath.Rel(idx.root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel != "." && (strings.Count(filepath.ToSlash(rel), "/") >= maxIndexDepth || skipIndexDir(entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			stats.Skipped++
			return nil
		}
		if strings.Count(filepath.ToSlash(rel), "/") >= maxIndexDepth {
			stats.Skipped++
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxIndexFileSize {
			stats.Skipped++
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, copyErr := io.Copy(h, io.LimitReader(f, maxIndexFileSize+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n > maxIndexFileSize {
			stats.Skipped++
			return nil
		}
		records = append(records, fileRecord{Path: filepath.ToSlash(rel), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		stats.Files++
		stats.Bytes += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}

func skipIndexDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".hg", ".svn":
		return true
	}
	return false
}
