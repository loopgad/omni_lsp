package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// §C11/§I17/§I18 diagnostic pipeline. Push (publishDiagnostics) is debounced
// per URI; pull (textDocument/diagnostic) shares the same bounded cache. The
// cache key carries every input the spec mandates: snapshot revision, content
// hash, BuildContextID, backend epoch, and a diagnostics-configuration hash.

const (
	diagDebounce      = 150 * time.Millisecond
	diagCacheLimit    = 512 // §K0: every cache is bounded (FIFO)
	diagConfigVersion = "v1"
	diagRetryLimit    = 3
)

var errDiagnosticDocumentClosed = errors.New("diagnostic document is closed")

// diagKey is the §C11 cache identity for one document's diagnostic set.
type diagKey struct {
	URI                   string
	SnapshotRev           uint64
	ContentHash           string // first 16 hex chars of sha256
	BuildContext          string
	BackendEpoch          uint64
	WorkspaceGeneration   uint64
	DiagnosticsGeneration uint64
	ConfigHash            string
}

func computeDiagKey(uri string, snapRev uint64, content []byte, bc string, epoch, workspaceGeneration, diagnosticsGeneration uint64) diagKey {
	sum := sha256.Sum256(content)
	return diagKey{
		URI:                   uri,
		SnapshotRev:           snapRev,
		ContentHash:           hex.EncodeToString(sum[:8]),
		BuildContext:          bc,
		BackendEpoch:          epoch,
		WorkspaceGeneration:   workspaceGeneration,
		DiagnosticsGeneration: diagnosticsGeneration,
		ConfigHash:            diagConfigVersion,
	}
}

// diagEntry pairs the cached set with its key so pull can revalidate.
type diagEntry struct {
	key     diagKey
	items   []languages.Diagnostic
	version int64
}

// diagCoordinator owns debounce timers, the bounded cache, and wire emission.
type diagCoordinator struct {
	mu       sync.Mutex
	cache    map[string]diagEntry // keyed by URI; latest set wins (FIFO order below)
	order    []string
	updates  map[string]uint64
	timers   map[string]*time.Timer
	runs     map[string]*diagRun
	pullRuns map[string]map[*diagRun]struct{}
	server   *Server
	closed   bool

	// plugin lane (§O0-O5 diagnostics.emit)：插件诊断与后端计算共用同一
	// 治理面——有界 FIFO 表、diagDebounce 去抖、宿主强制来源标记。键为
	// uri → pluginID → 该插件对该 uri 的最新 emit 集。
	pluginItems  map[string]map[string]pluginDiagSet
	pluginOrder  []string
	pluginTimers map[string]*time.Timer
}

// pluginDiagSet 是一个插件对单一 uri 的最新 emit 集（injectPluginDiagnostics
// 覆盖写：同插件同 uri 新 emit 顶替旧集，与 LSP 全量报告语义一致）。
type pluginDiagSet struct {
	items []languages.Diagnostic
}

// pluginSetsSnapshotLocked 返回 uri 名下全部插件 emit 集的快照（d.mu 持锁段
// 内调用，与 schedulePluginPush 回调的快照化同一纪律——发布读侧绝不见
// live inner map）。
func (d *diagCoordinator) pluginSetsSnapshotLocked(uri string) map[string]pluginDiagSet {
	live := d.pluginItems[uri]
	if len(live) == 0 {
		return nil
	}
	out := make(map[string]pluginDiagSet, len(live))
	for id, set := range live {
		out[id] = set
	}
	return out
}

// mergePublishItems 把后端计算集与插件 emit 集合并为单一全量报告的条目序：
// 后端在前、插件按 pluginID 确定性排序在后（与 publishPluginDiagnostics 的
// 既有排序一致）。第 18 轮治理开口 3：publishDiagnostics 是 uri 级全量替换
// ——两个车道对同一 uri 各自发布独立 payload 时互相顶掉（客户端只看得见
// 后到的一帧），合并是唯一保真形态。
func mergePublishItems(backend []languages.Diagnostic, sets map[string]pluginDiagSet) []languages.Diagnostic {
	ids := make([]string, 0, len(sets))
	for id := range sets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	merged := backend
	for _, id := range ids {
		merged = append(merged, sets[id].items...)
	}
	return merged
}

type diagRun struct {
	cancel context.CancelFunc
}

func newDiagCoordinator(s *Server) *diagCoordinator {
	return &diagCoordinator{
		cache:        make(map[string]diagEntry),
		updates:      make(map[string]uint64),
		timers:       make(map[string]*time.Timer),
		runs:         make(map[string]*diagRun),
		pullRuns:     make(map[string]map[*diagRun]struct{}),
		server:       s,
		pluginItems:  make(map[string]map[string]pluginDiagSet),
		pluginTimers: make(map[string]*time.Timer),
	}
}

// request schedules a debounced push for the URI (§C11 push path).
func (d *diagCoordinator) request(uri string) {
	d.schedule(uri, false)
}

// diagnosticsUpdated invalidates cached results and schedules a push when a
// nested server publishes a newer complete set for the current document.
func (d *diagCoordinator) diagnosticsUpdated(uri string) {
	d.schedule(uri, true)
}

// injectPluginDiagnostics 接收一次插件 diagnostics.emit（调用前已过 Grant
// 能力门，§O2）：入插件诊断表（FIFO 有界，与后端缓存同限）并按 diagDebounce
// 去抖后推送——插件 emit 不直连 notifyClient（diagnostics.go:282 路径的
// 勘察警告），而是走协调器自己的去抖与来源治理面。
//
// 关闭态守卫（复证圈 1 审计发现 1）：文档已关闭（vfs.Get==nil）的迟到 emit
// 即拒——与 compute 车道的 errDiagnosticDocumentClosed 拒绝（compute 入口、
// 发布前、提交前三处 vfs.Get==nil 检查）对齐。旧实现插件车道没有这道守卫：
// didCloseDocument 之后迟到的插件通知会复填充插件诊断表，并去抖上线对已
// 关闭文档的 publishDiagnostics。丢弃计入可见拒绝计数（pluginEmitRejected）。
// vfs.Get 在 d.mu 持锁段内调用是本文件既有模式（compute 的提交前守卫同）。
func (d *diagCoordinator) injectPluginDiagnostics(pluginID, uri string, items []languages.Diagnostic) {
	uri = canonicalDocumentURI(uri)
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	if d.server.vfs.Get(uri) == nil {
		d.mu.Unlock()
		d.server.pluginEmitRejected.Add(1)
		return
	}
	// seen 判据 O(1)（复证圈 2 审计发现 10）：pluginItems 的键存在性即
	// order 成员资格——order↔map 键集不变量在每个变更点同步维护（注入配对
	// 增删、didCloseDocument 对 order 与 map 的双重清理、FIFO 驱逐成对、
	// Close 双清空），由 TestPluginOrder_FilteredOnDocumentClose 钉住。圈 1
	// 修复曾以 order 线性扫描作判据（当时不变量刚由过滤修复确立）；不变量
	// 确立后，每次 emit 的 O(diagCacheLimit=512) 线性扫描不再必要。嵌套的
	// vfs.Get 有意保留在 d.mu 临界区内：提到锁外会让「关闭竞态」依赖上线前
	// 复查兜底而重新打开陈旧集复填窗口。
	if _, seen := d.pluginItems[uri]; !seen {
		d.pluginOrder = append(d.pluginOrder, uri)
		for len(d.pluginOrder) > diagCacheLimit { // FIFO eviction，与后端缓存同限
			old := d.pluginOrder[0]
			d.pluginOrder = d.pluginOrder[1:]
			delete(d.pluginItems, old)
		}
	}
	if d.pluginItems[uri] == nil {
		d.pluginItems[uri] = make(map[string]pluginDiagSet)
	}
	d.pluginItems[uri][pluginID] = pluginDiagSet{items: items}
	d.mu.Unlock()
	d.schedulePluginPush(uri)
}

// schedulePluginPush 以 diagDebounce 去抖一次插件诊断推送（重复 emit 重置
// 计时器——与后端 push 路径同一节奏）。
func (d *diagCoordinator) schedulePluginPush(uri string) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	if t := d.pluginTimers[uri]; t != nil {
		t.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(diagDebounce, func() {
		d.mu.Lock()
		if d.pluginTimers[uri] != timer || d.closed {
			d.mu.Unlock()
			return
		}
		delete(d.pluginTimers, uri)
		live, ok := d.pluginItems[uri]
		var sets map[string]pluginDiagSet
		if ok {
			// 快照化（复证圈 2 审计发现 7）：在 d.mu 临界区内把 inner map 拍成
			// 局部副本再解锁。旧实现把 live 引用直接交给发布读侧无锁迭代——
			// 与 injectPluginDiagnostics 对同一 inner map 的写入时间重叠即触发
			// Go 运行时不可恢复 fatal（concurrent map iteration and map
			// write），整个 LSP 进程崩溃。值类型 pluginDiagSet 按值拷贝，其
			// items 切片在注入侧只做整体替换不就地改写，快照共享安全。
			sets = make(map[string]pluginDiagSet, len(live))
			for id, set := range live {
				sets[id] = set
			}
		}
		d.mu.Unlock()
		if sets != nil {
			d.publishPluginDiagnostics(uri, sets)
		}
	})
	d.pluginTimers[uri] = timer
	d.mu.Unlock()
}

// publishPluginDiagnostics 推送该 uri 的合并诊断集（第 18 轮治理开口 3）：
// 后端 compute 车道在 §C11 缓存中的当前集 + 全部插件的最新 emit 集（按插件
// ID 确定性排序）合并为单一全量报告后经 notifyClient 上线——publishDiagnostics
// 是 uri 级全量替换，插件车道独占发布会把后端集整体顶掉（反之亦然），因此
// 两个上线点（本函数与 publishComputed）都在发布前合并对方当前集。每条插件
// 诊断的 Source 已在注入时被宿主强制为 "plugin:<id>"——线协议上来源可见、
// 可治理。后端缓存无该 uri 条目（后端从未算过/FIFO 驱逐）时只带插件集，
// version 字段随缓存条目缺席一并省略（与既有插件 payload 形状一致）。
//
// 与后端集的合并读在 d.mu 临界区内取快照（与插件集快照化同一纪律）；后端
// 缓存集可能是正在被更新代替换的上一代——发布全量报告的收敛语义由两条车道
// 的下一次去抖推送兜住（后到者同样合并对方），瞬时陈旧无害。
func (d *diagCoordinator) publishPluginDiagnostics(uri string, sets map[string]pluginDiagSet) {
	d.mu.Lock()
	backendItems := []languages.Diagnostic(nil)
	version := int64(0)
	hasBackend := false
	if en, ok := d.cache[uri]; ok {
		backendItems = en.items
		version = en.version
		hasBackend = true
	}
	stillOpen := !d.closed && d.server.vfs.Get(uri) != nil
	d.mu.Unlock()
	if !stillOpen {
		return
	}
	items := mergePublishItems(backendItems, sets)
	displayURI := uri
	if file := d.server.vfs.Get(uri); file != nil {
		displayURI = file.URI
	}
	params := map[string]any{
		"uri":         displayURI,
		"diagnostics": projectDiagnostics(items),
	}
	if hasBackend {
		params["version"] = version
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return
	}
	// 上线前关闭态复查（复证圈 1 审计发现 1）：快照与真正上线之间文档可能
	// 已关闭（didCloseDocument 不等待去抖回调），marshal 是两者之间的慢段
	// ——已关闭文档不得再收到诊断推送。与 publishComputed 的「上线前复查」
	// 同一纪律。
	d.mu.Lock()
	stillOpen = !d.closed && d.server.vfs.Get(uri) != nil
	d.mu.Unlock()
	if !stillOpen {
		return
	}
	d.server.notifyClient("textDocument/publishDiagnostics", json.RawMessage(payload))
}

func (d *diagCoordinator) schedule(uri string, childUpdate bool) {
	uri = canonicalDocumentURI(uri)
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	if childUpdate {
		d.updates[uri]++
		delete(d.cache, uri)
		filtered := d.order[:0]
		for _, queued := range d.order {
			if queued != uri {
				filtered = append(filtered, queued)
			}
		}
		d.order = filtered
	}
	defer d.mu.Unlock()
	if t := d.timers[uri]; t != nil {
		t.Stop()
	}
	if run := d.runs[uri]; run != nil {
		run.cancel()
		delete(d.runs, uri)
	}
	ready := make(chan struct{})
	var timer *time.Timer
	timer = time.AfterFunc(diagDebounce, func() {
		<-ready
		d.mu.Lock()
		if d.timers[uri] != timer {
			d.mu.Unlock()
			return
		}
		delete(d.timers, uri)
		ctx, cancel := context.WithCancel(context.Background())
		run := &diagRun{cancel: cancel}
		d.runs[uri] = run
		d.mu.Unlock()
		d.computeAndPush(ctx, uri)
		cancel()
		d.mu.Lock()
		if d.runs[uri] == run {
			delete(d.runs, uri)
		}
		d.mu.Unlock()
	})
	d.timers[uri] = timer
	close(ready)
}

// didClose cancels pending and in-flight push work and removes the bounded
// per-URI diagnostic cache before the VFS drops the editor document.
func (d *diagCoordinator) didClose(uri string) {
	d.didCloseDocument(uri, nil)
}

// didCloseDocument serializes the VFS close with diagnostic cache commits.
// A computation either commits before this transition and is cleared here, or
// observes the closed VFS entry and cannot repopulate the cache afterward.
func (d *diagCoordinator) didCloseDocument(uri string, closeDocument func()) {
	uri = canonicalDocumentURI(uri)
	d.mu.Lock()
	defer d.mu.Unlock()
	if timer := d.timers[uri]; timer != nil {
		timer.Stop()
		delete(d.timers, uri)
	}
	if run := d.runs[uri]; run != nil {
		run.cancel()
		delete(d.runs, uri)
	}
	for run := range d.pullRuns[uri] {
		run.cancel()
		delete(d.pullRuns[uri], run)
	}
	delete(d.pullRuns, uri)
	if timer := d.pluginTimers[uri]; timer != nil {
		timer.Stop()
		delete(d.pluginTimers, uri)
	}
	delete(d.pluginItems, uri)
	// pluginOrder 与 d.order 同一原地过滤（复证圈 1 审计发现 2）：旧实现只
	// 清 pluginItems/pluginTimers，close/reopen 循环后同一 uri 在 pluginOrder
	// 累积重复槽位——FIFO 驱逐据 order 长度删 map 时会误删存活 uri 的插件
	// 诊断集。
	filteredPluginOrder := d.pluginOrder[:0]
	for _, queued := range d.pluginOrder {
		if queued != uri {
			filteredPluginOrder = append(filteredPluginOrder, queued)
		}
	}
	d.pluginOrder = filteredPluginOrder
	if closeDocument != nil {
		closeDocument()
	}
	delete(d.cache, uri)
	delete(d.updates, uri)
	filtered := d.order[:0]
	for _, queued := range d.order {
		if queued != uri {
			filtered = append(filtered, queued)
		}
	}
	d.order = filtered
}

// Close cancels pending debounce timers (session teardown).
func (d *diagCoordinator) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = map[string]*time.Timer{}
	for _, run := range d.runs {
		run.cancel()
	}
	d.runs = map[string]*diagRun{}
	for _, runs := range d.pullRuns {
		for run := range runs {
			run.cancel()
		}
	}
	d.pullRuns = map[string]map[*diagRun]struct{}{}
	for _, t := range d.pluginTimers {
		t.Stop()
	}
	d.pluginTimers = map[string]*time.Timer{}
	d.pluginItems = map[string]map[string]pluginDiagSet{}
	d.pluginOrder = nil
}

// computeAndPush runs diagnostics and emits publishDiagnostics.
func (d *diagCoordinator) computeAndPush(ctx context.Context, uri string) {
	uri = canonicalDocumentURI(uri)
	key, items, err := d.compute(ctx, uri)
	if err != nil || ctx.Err() != nil {
		if err != nil && ctx.Err() == nil {
			d.server.recordEvidence(ctx, "textDocument/publishDiagnostics", uri, identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
		}
		return
	}
	be := d.server.findWorkspaceBackendFor(uri)
	if be == nil {
		return
	}
	d.publishComputed(uri, key, items, be)
}

// publishComputed emits publishDiagnostics for a freshly computed (key, items)
// pair — but only while that pair is still the coordinator's current
// generation for uri. Both guards compare against the key this compute run
// actually produced, closing the stale-publish window where a paused older
// run reaches the emit point after a newer run has overwritten the cache: the
// previous code compared only cache-entry-vs-current-input and never
// cache-entry-vs-this-run, so the old run's items could go out on the wire
// under the new run's version. The checks run before the payload is built and
// again immediately before the wire write (marshal is the slow part between
// them).
func (d *diagCoordinator) publishComputed(uri string, key diagKey, items []languages.Diagnostic, be languages.Backend) {
	d.mu.Lock()
	cached, ok := d.cache[uri]
	// 插件集快照（第 18 轮治理开口 3）：后端全量报告必须带上插件车道对该
	// uri 的当前 emit 集，否则后端发布会把插件诊断整体顶掉（LSP uri 级全量
	// 替换）。快照在 d.mu 临界区内拍平（发布读侧绝不见 live inner map）；
	// marshal 慢段内插件再 emit 的增量由插件车道的下一次去抖推送收敛。
	pluginSnap := d.pluginSetsSnapshotLocked(uri)
	d.mu.Unlock()
	if !ok || cached.key != key {
		// The cache no longer holds this run's entry: a newer generation has
		// overwritten it (or didClose/FIFO eviction removed it). version and
		// items could no longer be bound to one generation — never publish.
		return
	}
	if cur := d.currentKey(uri, be); cur != key {
		return
	}
	version := cached.version
	items = mergePublishItems(items, pluginSnap)
	wire := projectDiagnostics(items)
	displayURI := uri
	if file := d.server.vfs.Get(uri); file != nil {
		displayURI = file.URI
	}
	payload, merr := json.Marshal(map[string]any{
		"uri":         displayURI,
		"version":     version,
		"diagnostics": wire,
	})
	if merr != nil {
		return
	}
	d.mu.Lock()
	cached, ok = d.cache[uri]
	d.mu.Unlock()
	if !ok || cached.key != key {
		return
	}
	// 上线前复查的廉价化（复证圈 2 审计发现 12）：此处只比较身份字段，不再
	// 整文档重拷贝 + 全内容 sha256。等价性：内容变更必经 publishSnapshot 推进
	// SnapshotRev（vfs.Update/Save/Close 各自 AdvanceRevision，handlers.go 的
	// publishSnapshot 以 vfs.Revision() 发布快照），「ContentHash 单独变化而
	// 身份字段全同」的组合在生产路径不可达；marshal 前的全量 currentKey 复查
	// （本函数上方）保留完整保真度，旧实现的逐发布 2 份全量拷贝 + 3 次全内容
	// 哈希降为 1 份 + 1 次。
	if cur := d.currentKeyIdentity(uri, be); !sameKeyIdentity(cur, key) {
		return
	}
	d.server.notifyClient("textDocument/publishDiagnostics", json.RawMessage(payload))
}

// currentKeyIdentity 是 currentKey 的廉价口径：只计算身份字段（URI、快照
// 修订、构建上下文、backend epoch、工作区代、诊断代、配置哈希），不做整文档
// 拷贝与全内容 sha256（复证圈 2 审计发现 12）。ContentHash 字段留零值——
// 仅配 sameKeyIdentity 使用，不得与完整 diagKey 直接比较。
func (d *diagCoordinator) currentKeyIdentity(uri string, be languages.Backend) diagKey {
	uri = canonicalDocumentURI(uri)
	rev := uint64(0)
	if snap := d.server.snapMgr.Current(); snap != nil {
		rev = snap.ID().Revision
	}
	d.mu.Lock()
	diagnosticsGeneration := d.updates[uri]
	d.mu.Unlock()
	return diagKey{
		URI:                   uri,
		SnapshotRev:           rev,
		BuildContext:          string(backendBuildContext(be)),
		BackendEpoch:          backendEpoch(be),
		WorkspaceGeneration:   backendWorkspaceGeneration(be),
		DiagnosticsGeneration: diagnosticsGeneration,
		ConfigHash:            diagConfigVersion,
	}
}

// sameKeyIdentity 比较 diagKey 的身份字段（不含 ContentHash——键的其余字段
// 全同时，内容哈希由 compute 的缓存键与 marshal 前的 currentKey 全量复查承担）。
func sameKeyIdentity(a, b diagKey) bool {
	return a.URI == b.URI && a.SnapshotRev == b.SnapshotRev &&
		a.BuildContext == b.BuildContext && a.BackendEpoch == b.BackendEpoch &&
		a.WorkspaceGeneration == b.WorkspaceGeneration &&
		a.DiagnosticsGeneration == b.DiagnosticsGeneration &&
		a.ConfigHash == b.ConfigHash
}

func (d *diagCoordinator) currentKey(uri string, be languages.Backend) diagKey {
	uri = canonicalDocumentURI(uri)
	src := d.server.vfs.Content(uri)
	if src == nil {
		src = []byte{}
	}
	rev := uint64(0)
	if snap := d.server.snapMgr.Current(); snap != nil {
		rev = snap.ID().Revision
	}
	d.mu.Lock()
	diagnosticsGeneration := d.updates[uri]
	d.mu.Unlock()
	return computeDiagKey(uri, rev, src, string(backendBuildContext(be)), backendEpoch(be), backendWorkspaceGeneration(be), diagnosticsGeneration)
}

// compute resolves diagnostics through the bounded §C11 cache. It returns the
// key actually bound to the returned items — the cache entry key on a hit, the
// committed resultKey otherwise — so the push path can validate at emit time
// that what it is about to publish is still this run's generation.
func (d *diagCoordinator) compute(ctx context.Context, uri string) (pushKey diagKey, result []languages.Diagnostic, resultErr error) {
	uri = canonicalDocumentURI(uri)
	if d.server.vfs.Get(uri) == nil {
		return diagKey{}, nil, errDiagnosticDocumentClosed
	}
	be := d.server.findWorkspaceBackendFor(uri)
	if be == nil {
		return diagKey{}, nil, fmt.Errorf("no backend for %s", uri)
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil && d.server.snapMgr != nil {
		captured = d.server.snapMgr.Current()
	}
	var src []byte
	snapRev := uint64(0)
	if captured != nil {
		snapRev = captured.ID().Revision
		doc := captured.Document(uri)
		if doc == nil {
			return diagKey{}, nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "requested document is absent from the captured workspace snapshot")
		}
		src = doc.Content
	} else {
		src = d.server.vfs.Content(uri)
	}
	if src == nil {
		src = []byte{}
	}
	leaseCtx, finishSnapshot, err := d.server.beginBackendWorkspaceSnapshot(ctx, be, captured)
	if err != nil {
		return diagKey{}, nil, err
	}
	defer func() {
		if finishSnapshot != nil {
			if finishErr := finishSnapshot(); finishErr != nil {
				result, resultErr = nil, finishErr
			}
		}
	}()
	workspaceGeneration := backendWorkspaceGeneration(be)
	epoch := backendEpoch(be)
	buildContext := string(backendBuildContext(be))

	d.mu.Lock()
	diagnosticsGeneration := d.updates[uri]
	d.mu.Unlock()
	key := computeDiagKey(uri, snapRev, src, buildContext, epoch, workspaceGeneration, diagnosticsGeneration)

	d.mu.Lock()
	if en, ok := d.cache[uri]; ok && en.key == key {
		d.mu.Unlock()
		return en.key, en.items, nil
	}
	d.mu.Unlock()

	// Both in-process overlays and nested diagnostic refresh must consume the
	// captured workspace context. Nested refresh can update its diagnostic
	// version inside this lease without changing the source generation.
	if err := ctx.Err(); err != nil {
		return diagKey{}, nil, err
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return diagKey{}, nil, err
		}
		var items []languages.Diagnostic
		if encoded, ok := be.(languages.EncodedDiagnosticsProvider); ok {
			items, err = encoded.DiagnosticsWithEncoding(leaseCtx, uri, src, snapRev, d.server.negotiatedEncodingInt())
		} else {
			items, err = be.Diagnostics(leaseCtx, uri, src)
		}
		if err != nil {
			return diagKey{}, nil, err
		}
		if ctx.Err() != nil {
			return diagKey{}, nil, ctx.Err()
		}
		if d.server.vfs.Get(uri) == nil {
			return diagKey{}, nil, errDiagnosticDocumentClosed
		}
		if string(backendBuildContext(be)) != buildContext || backendEpoch(be) != epoch {
			return diagKey{}, nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "backend context changed while diagnostics were computed")
		}
		if backendWorkspaceGeneration(be) != workspaceGeneration {
			return diagKey{}, nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "child workspace changed while diagnostics were computed")
		}

		d.mu.Lock()
		if ctx.Err() != nil {
			d.mu.Unlock()
			return diagKey{}, nil, ctx.Err()
		}
		if d.server.vfs.Get(uri) == nil {
			d.mu.Unlock()
			return diagKey{}, nil, errDiagnosticDocumentClosed
		}
		if d.updates[uri] != diagnosticsGeneration {
			// A child report can replace the data after Diagnostics has copied
			// its result but before this commit. The result is then ambiguous:
			// fetch again under the new generation instead of caching it as new.
			diagnosticsGeneration = d.updates[uri]
			d.mu.Unlock()
			if attempt+1 == diagRetryLimit {
				return diagKey{}, nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "child diagnostics kept changing while this report was computed")
			}
			continue
		}
		if _, seen := d.cache[uri]; !seen {
			d.order = append(d.order, uri)
			for len(d.cache) >= diagCacheLimit && len(d.order) > 0 { // FIFO eviction
				old := d.order[0]
				d.order = d.order[1:]
				delete(d.cache, old)
			}
		}
		version := int64(0)
		if captured := snapshotFromCtx(ctx); captured != nil {
			if doc := captured.Document(uri); doc != nil {
				version = doc.Version
			}
		} else if f := d.server.vfs.Get(uri); f != nil {
			version = f.Version
		}
		resultKey := computeDiagKey(uri, snapRev, src, buildContext, epoch, workspaceGeneration, diagnosticsGeneration)
		d.cache[uri] = diagEntry{key: resultKey, items: items, version: version}
		d.mu.Unlock()
		return resultKey, items, nil
	}
}

// computePull tracks request-scoped diagnostic computation so didClose can
// cancel it and guarantee that a pull started for a closing document cannot
// refill the cache after the close transition.
func (d *diagCoordinator) computePull(parent context.Context, uri string) ([]languages.Diagnostic, error) {
	uri = canonicalDocumentURI(uri)
	ctx, cancel := context.WithCancel(parent)
	run := &diagRun{cancel: cancel}
	d.mu.Lock()
	if d.server.vfs.Get(uri) == nil {
		d.mu.Unlock()
		cancel()
		return nil, errDiagnosticDocumentClosed
	}
	if d.pullRuns[uri] == nil {
		d.pullRuns[uri] = make(map[*diagRun]struct{})
	}
	d.pullRuns[uri][run] = struct{}{}
	d.mu.Unlock()
	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.pullRuns[uri], run)
		if len(d.pullRuns[uri]) == 0 {
			delete(d.pullRuns, uri)
		}
		d.mu.Unlock()
	}()

	// The pull path answers from the same compute run but has no generation
	// identity of its own to publish; the returned key binding is irrelevant
	// here (cache revalidation happens per request).
	_, items, err := d.compute(ctx, uri)
	if err != nil {
		if errors.Is(err, context.Canceled) && d.server.vfs.Get(uri) == nil {
			return nil, errDiagnosticDocumentClosed
		}
		return nil, err
	}
	if d.server.vfs.Get(uri) == nil {
		return nil, errDiagnosticDocumentClosed
	}
	return items, nil
}

// findWorkspaceBackendFor resolves diagnostics through the same URI-to-backend
// path used by semantic requests. In particular, language IDs such as
// javascriptreact and typescriptreact are routed by their file extension to
// the TypeScript backend. An unresolved URI must not fall through to an
// arbitrary registered backend.
func (s *Server) findWorkspaceBackendFor(uri string) languages.Backend {
	be, err := s.resolveBackend(uri)
	if err != nil {
		return nil
	}
	return be
}

// backendEpoch reports the backend's restart epoch when it exposes one
// (nested bridges); in-process backends are stable at 0.
func backendEpoch(be languages.Backend) uint64 {
	type epoker interface{ SupervisorEpoch() uint64 }
	if e, ok := be.(epoker); ok {
		return e.SupervisorEpoch()
	}
	return 0
}

// resultID derives the §I18 stable identity from semantic fields — source,
// code, message, and the intra-document range. Two diagnostics with the same
// semantic content keep the same ID across line shifts, reducing editor flicker.
func resultID(item languages.Diagnostic) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d:%d:%d:%d",
		item.Source, item.Code, item.Message,
		item.StartLine, item.StartChar, item.EndLine, item.EndChar)))
	return hex.EncodeToString(h[:12])
}

func diagnosticsResultID(items []languages.Diagnostic) string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, resultID(item))
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// projectDiagnostics converts internal diagnostics to the LSP wire shape.
func projectDiagnostics(items []languages.Diagnostic) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		entry := map[string]any{
			"range": map[string]any{
				"start": map[string]any{"line": it.StartLine, "character": it.StartChar},
				"end":   map[string]any{"line": it.EndLine, "character": it.EndChar},
			},
			"severity": it.Severity,
			"source":   it.Source,
			"message":  it.Message,
			"code":     it.Code,
		}
		out = append(out, entry)
	}
	return out
}

// handlePullDiagnostics serves textDocument/diagnostic (§C11 pull path).
//
// Failure contract: only ContentModified and client/clock cancellation answer
// with a typed error the client already understands, and only a closed or
// absent document answers an empty full report — that report is truthful for a
// document with no open editor state. Every other computation failure is
// returned as a JSON-RPC error (InternalError via the dispatcher): under pull
// semantics the client replaces its diagnostic set with whatever a full report
// carries, so answering an empty report on a transient backend failure would
// erase every diagnostic the editor is showing. Returning the error instead
// makes the client keep its previous set, mirroring the push path where a
// failed computation simply does not publish (§Q4: no silent fallback — the
// failure is visible both as the error response and in backend-failure
// metrics).
func (s *Server) handlePullDiagnostics(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid diagnostic params: %w", err)
	}
	items, err := s.diag.computePull(ctx, params.TextDocument.URI)
	if err != nil {
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &jsonrpc.ResponseError{
				Code:    jsonrpc.RequestCancelled,
				Message: "diagnostic pull cancelled",
			}
		}
		if !errors.Is(err, errDiagnosticDocumentClosed) {
			s.metrics.BackendFails.Inc(1)
			return nil, err
		}
		// Closed/absent document: the empty full report below is the
		// truthful answer (§Q4), and no backend work was spent.
		items = nil
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":     "full",
		"resultId": diagnosticsResultID(items),
		"items":    projectDiagnostics(items),
	})
	return payload, nil
}
