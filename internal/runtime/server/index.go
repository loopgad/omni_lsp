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
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const inventoryVersion = "file-inventory-v1"
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

// IndexStats is the C12 read-only status projection.
type IndexStats struct {
	Enabled         bool                    `json:"enabled"`
	Reason          string                  `json:"reason,omitempty"`
	IndexDir        string                  `json:"indexDir,omitempty"`
	Generation      uint64                  `json:"generation"`
	Segments        []persistent.SegmentRef `json:"segments"`
	Quarantined     []persistent.SegmentID  `json:"quarantined"`
	DiskBudgetBytes int64                   `json:"diskBudgetBytes"`
	LastRebuild     IndexRebuildStats       `json:"lastRebuild"`
	Fresh           bool                    `json:"fresh"`
	Revision        uint64                  `json:"revision"`
}

type indexService struct {
	root    string
	dir     string
	store   *persistent.FileStore
	budget  int64
	buildMu sync.Mutex
	statsMu sync.RWMutex
	last    IndexRebuildStats
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
	s.idx = &indexService{root: root, dir: dir, store: store, budget: s.config.IndexDiskBudgetBytes}
	s.idxReason = ""
	s.trust = policy
	s.mu.Unlock()
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
	idx.statsMu.RLock()
	stats.LastRebuild = idx.last
	idx.statsMu.RUnlock()
	view, err := idx.store.OpenSnapshot(ctx)
	stats.Quarantined = idx.store.Quarantined()
	if errors.Is(err, persistent.ErrNoGeneration) {
		return json.Marshal(stats)
	}
	if err != nil {
		stats.Reason = err.Error()
		return json.Marshal(stats)
	}
	stats.Generation, stats.Segments = view.ID, view.Segments
	if len(view.Segments) == 1 {
		sealed, readErr := view.ReadSegment(view.Segments[0].ID)
		if readErr == nil {
			_, readErr = persistent.VerifyPayload(sealed, persistent.FreshnessTuple{BackendVer: inventoryVersion, Revision: stats.Revision})
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
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: fmt.Sprintf("reindex rejected: %v", gateErr)}
	}
	if !idx.buildMu.TryLock() {
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "reindex already running"}
	}
	defer idx.buildMu.Unlock()
	started := time.Now()
	revision := s.currentRevision()
	last := IndexRebuildStats{}
	defer func() {
		last.DurationMs = time.Since(started).Milliseconds()
		idx.statsMu.Lock()
		idx.last = last
		idx.statsMu.Unlock()
	}()
	records, err := idx.inventory(ctx, &last)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		last.Error = err.Error()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: err.Error()}
	}
	data, err := json.Marshal(records)
	if err != nil {
		last.Error = err.Error()
		return nil, err
	}
	build, err := idx.store.BeginBuild(ctx)
	if err != nil {
		last.Error = err.Error()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: err.Error()}
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	_, err = build.WriteSegment(persistent.SealPayload(data, persistent.FreshnessTuple{BackendVer: inventoryVersion, Revision: revision}))
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && s.currentRevision() != revision {
		err = fmt.Errorf("workspace changed during reindex (snapshot %d, current %d)", revision, s.currentRevision())
	}
	if err == nil {
		err = build.Commit(ctx)
	}
	if err != nil {
		last.Error = err.Error()
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: err.Error()}
	}
	committed = true
	return json.Marshal(map[string]any{"generation": idxGeneration(ctx, idx.store), "files": last.Files, "bytes": last.Bytes, "skipped": last.Skipped, "revision": revision})
}

func idxGeneration(ctx context.Context, store *persistent.FileStore) uint64 {
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		return 0
	}
	return view.ID
}

func (idx *indexService) inventory(ctx context.Context, stats *IndexRebuildStats) ([]fileRecord, error) {
	records := make([]fileRecord, 0)
	err := filepath.WalkDir(idx.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == idx.dir && entry.IsDir() {
			return filepath.SkipDir
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
