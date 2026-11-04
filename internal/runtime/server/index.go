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
	"strconv"
	"strings"
	"sync"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/security"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// v2 binds persisted freshness to the canonical workspace root and disk tree;
// process-local snapshot revisions remain build guards only.
const inventoryVersion = "file-inventory-v2"
const maxIndexDepth = 16
const maxIndexFileSize = 1 << 20

// indexBudgetLadderEnv opts the persistent index into the goal.md §F13
// memory-budget ladder (DEF-BUDWIRE wiring). The value is the ladder's total
// budget in bytes. Unset or invalid keeps the ladder nil: the index path
// behaves byte-for-byte as before the wiring (X10: default behavior must not
// change local core).
const indexBudgetLadderEnv = "OMNILSP_INDEX_MEMORY_BUDGET_BYTES"

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
	// ladder is the goal.md §F13 BudgetManager over the index write path,
	// constructed only when indexBudgetLadderEnv opts in. Nil elsewhere:
	// every ladder touch must be nil-guarded (default = no-op, X10).
	ladder  *scheduler.BudgetManager
	buildMu sync.Mutex
	statsMu sync.RWMutex
	last    IndexRebuildStats
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
	// 复证圈 2 审计发现 2：构造期的跨进程 writer.lock 获取必须有界——无
	// deadline 的调用在另一进程持锁（reindex/Compact 长临界区）时无限等待，
	// initialize 握手挂死。超时走既有 disableIndex fail-open 分支（索引禁用、
	// 原因可观测、服务器进程照常运行）。
	lockCtx, cancel := context.WithTimeout(context.Background(), indexLadderRefreshTimeout)
	defer cancel()
	store, err := persistent.NewFileStoreContext(lockCtx, dir, persistent.Config{DiskBudgetBytes: s.config.IndexDiskBudgetBytes})
	if err != nil {
		s.disableIndex(err)
		return
	}
	ladder := newIndexBudgetLadder(s.config.IndexDiskBudgetBytes)
	s.mu.Lock()
	s.idx = &indexService{root: root, workspaceID: workspaceID, dir: dir, store: store, budget: s.config.IndexDiskBudgetBytes, ladder: ladder}
	s.idxReason = ""
	s.trust = policy
	s.mu.Unlock()
	if ladder != nil {
		// Seed the ladder with the index already on disk so a restarted
		// process gates at the real pressure instead of zero. Best-effort:
		// a failed seed leaves usage at zero (fail-open) while the
		// persistent store's WriteSegment budget stays the authoritative
		// hard stop.
		s.idx.refreshBudgetLadder()
	}
	// 工作区信任策略此刻已建立（审计批 2）：推进装配期延后的插件 Launch
	// （mountPlugins 只装配到 Enabled，launchReadyPlugins 过 plugin.load 门）。
	//
	// Round-23 OPT-3：launch 后台化。launchReadyPlugins 逐插件做 entrypoint
	// 整文件 sha256 复核（launchChecksumBudget 预算 30s）+ 子进程 spawn，此前
	// 同步运行在 dispatchInline 持有的 mutationMu 临界区内，initialize 应答被
	// 校验/launch 全程阻塞（实测 4 插件 × 32MB entrypoint ≈ 194ms）。就绪门
	// 语义不变：未 launch 的插件 emit 仍走既有拒绝/丢弃计数，信任裁决、关闭
	// 交错（pluginClosing 的起飞丢弃与起飞后 kill+Disable 拆除）、pending 幂等
	// 全部由 launchReadyPlugins 自身维持；ClosePlugins 置位后的后台起飞按
	// 已关闭处理，不会泄漏进程。
	go s.launchReadyPlugins()
}

// newIndexBudgetLadder builds the §F13 ladder for the index service when
// indexBudgetLadderEnv names a positive total budget in bytes; nil otherwise
// (default off — X10 red line). The persistent-index category is fed the
// configured disk budget, so CategoryPressure reads against the same ceiling
// the persistent store's WriteSegment enforces; the explicit total budget
// drives the pressure ladder itself. Invalid values announce on stderr and
// keep the ladder off instead of guessing (the positiveIntFromEnv
// convention).
func newIndexBudgetLadder(diskBudget int64) *scheduler.BudgetManager {
	text := strings.TrimSpace(os.Getenv(indexBudgetLadderEnv))
	if text == "" {
		return nil
	}
	total, err := strconv.ParseInt(text, 10, 64)
	if err != nil || total <= 0 {
		security.FprintfRedacted(os.Stderr, "omnilsp: ignoring invalid %s=%q (want a positive integer); the index budget ladder stays off\n", indexBudgetLadderEnv, text)
		return nil
	}
	ladder := scheduler.NewBudgetManager(scheduler.DefaultBudgetThresholds())
	ladder.SetLimit(scheduler.CatDynamicIndex, diskBudget)
	ladder.SetTotalBudget(total)
	return ladder
}

// refreshBudgetLadder resets the ladder's accounting to the committed
// generation's segment bytes — real byte accounting on the actual allocation
// point (DEF-BUDWIRE), not accumulation: retired generations shrink the
// recorded usage along with the store. With no generation on disk the usage
// clears to zero.
//
// manifest 只读口径（复证圈 2 审计发现 8）：段长总和来自
// FileStore.CurrentGenerationLenSum（s.mu 下对 current 求和）——旧实现为求和
// 走 OpenSnapshotLease，全代读盘+CRC 校验且独占跨进程 writer.lock，校验循环
// 对 deadline 盲（5s 有界包裹包错了段），initialize 关键路径因此做全量 I/O。
// 无 I/O 失败面后旧 fail-open 分支退化为「无代清零」（等价旧 ErrNoGeneration
// 分支）。ReplaceUsage 一次持锁完成「扣旧值→记新值」（圈 1 发现 6），并发
// 记账方与 refresh 交错不产生撕裂。
func (idx *indexService) refreshBudgetLadder() {
	if idx.ladder == nil {
		return
	}
	idx.ladder.ReplaceUsage(scheduler.CatDynamicIndex, idx.store.CurrentGenerationLenSum())
}

// indexBudgetRefusal shapes a §F13 ladder refusal as a visible reindex
// failure: the request errors, the rebuild stats carry the same message and
// nothing is published (rejection must not be silent).
func indexBudgetRefusal(message string) *jsonrpc.ResponseError {
	return &jsonrpc.ResponseError{
		Code:    jsonrpc.RequestFailed,
		Message: "reindex rejected: " + message,
		Data:    json.RawMessage(`{"kind":"budget_exceeded"}`),
	}
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
	security.FprintfRedacted(os.Stderr, "omnilsp: persistent index disabled: %v\n", err)
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
		// The semantic path streams an unknown number of segments through the
		// sink, so its ladder gate is the §F13 pressure check itself: a live
		// index already in the reject band (>= High, 90%) refuses the
		// expensive rebuild up front instead of after extraction. The
		// persistent store's WriteSegment budget stays the byte-level
		// authority on what actually lands on disk.
		if idx.ladder != nil && !idx.ladder.CanAllocate(0) {
			last.Error = fmt.Sprintf("F13 memory budget ladder in the reject band (pressure %.1f%%)", idx.ladder.Pressure()*100)
			return nil, indexBudgetRefusal(last.Error)
		}
		result, semanticErr := s.reindexSemantic(ctx, idx, revision, &last)
		if semanticErr != nil {
			last.Error = semanticErr.Error()
			return nil, reindexResponseError(semanticErr)
		}
		idx.refreshBudgetLadder()
		return result, nil
	}
	// 门在物化前布防（复证圈 2 审计发现 5）：inventory 流式按累计 payload
	// 估算超限即中止（fail-closed 可见拒绝）——inventoryPayload 的一次性
	// json.Marshal 不再为注定被拒的分配买单。物化后的精确 CanAllocate
	// （下方）保留为权威兜底（流式估算逐文件累计，阈值一致）。
	records, err := idx.inventoryWithGate(ctx, &last, func(cumulative int64) bool {
		return idx.ladder == nil || idx.ladder.CanAllocate(cumulative)
	})
	if err != nil {
		if errors.Is(err, errInventoryOverBudget) {
			msg := fmt.Sprintf("F13 memory budget ladder refuses an index inventory beyond its remaining budget (pressure %.1f%%)", idx.ladder.Pressure()*100)
			last.Error = msg
			return nil, indexBudgetRefusal(msg)
		}
		last.Error = err.Error()
		return nil, reindexResponseError(err)
	}
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
	// The legacy path knows the exact bytes it is about to stage, so the
	// ladder gate accounts them: a payload that would land the index at or
	// above the reject band is refused before any build starts (§F13 >= 90%).
	// This links the ladder to the write path without re-implementing the
	// persistent layer's budget-first check, which stays authoritative at
	// 100% of the disk budget.
	if idx.ladder != nil && !idx.ladder.CanAllocate(int64(len(data))) {
		last.Error = fmt.Sprintf("F13 memory budget ladder refuses a %d-byte index segment (pressure %.1f%%)", len(data), idx.ladder.Pressure()*100)
		return nil, indexBudgetRefusal(last.Error)
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
	idx.refreshBudgetLadder()
	return json.Marshal(map[string]any{"generation": generation, "files": last.Files, "bytes": last.Bytes, "skipped": last.Skipped, "revision": revision})
}

// firstInventoryChange names the first differing file between two inventories.
// Both inputs come from inventory(): sorted by path (sort.Slice above) and
// duplicate-free (WalkDir visits each path once), and fileRecord carries only
// the three compared fields, so differing json.Marshal digests — the only
// condition under which callers invoke this — always surface as an added,
// removed or changed record that the merge loop returns before both cursors
// exhaust. Reaching the tail therefore means an invariant broke (unsorted
// input, a widened fileRecord); Go does no exhaustiveness analysis here, and a
// silent fallback would disguise that corruption as a legitimate message, so
// it fails loudly like the round-five completenessName arm. A panic stays a
// per-request error: the scheduler recovers it (executeSafely).
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
	panic("unreachable: inventory digests differ but no file record differs")
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
	return idx.inventoryWithGate(ctx, stats, nil)
}

// errInventoryOverBudget 是 inventoryWithGate 的预算中止信号（复证圈 2 审计
// 发现 5）：累计 payload 估算超过 §F13 阶梯剩余额度时逐文件流式中止——旧
// 实现整量物化后才 CanAllocate，门防不住它要门控的那次分配。
var errInventoryOverBudget = errors.New("inventory budget exceeded")

// inventoryWithGate 在 inventory 的逐文件循环上接预算闸门：每收录一个文件
// 累计其 payload 估算字节（JSON 字段名与结构开销的保守余量，估算略高即早拒
// ——门语义保守，物化后的精确 CanAllocate 仍是权威兜底），admit 返回 false
// 即流式中止（fail-closed 可见拒绝）。admit 为 nil 等价无闸门。
func (idx *indexService) inventoryWithGate(ctx context.Context, stats *IndexRebuildStats, admit func(cumulative int64) bool) ([]fileRecord, error) {
	records := make([]fileRecord, 0)
	var cumulative int64
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
		if admit != nil {
			last := records[len(records)-1]
			cumulative += int64(len(last.Path) + len(last.SHA256) + 96)
			if !admit(cumulative) {
				return errInventoryOverBudget
			}
		}
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
