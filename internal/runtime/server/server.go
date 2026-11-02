// Package server implements the LSP JSON-RPC server for OmniLSP.
//
// Responsibility:
//
//	Accepts JSON-RPC messages via a transport, enforces the C2 lifecycle gate,
//	classifies priority (F3), admits queries through the scheduler (F5/F6),
//	dispatches to registered handlers, and projects canonical results to
//	protocol responses.
//
// Owned mutable state:
//
//	state (State enum, protected by mu), transport (set at Run), languages map,
//	inflight table, evidence ring (both mu-protected), syncRejects (atomic).
//
// Concurrency model:
//
//	sync.RWMutex for state/transport/inflight/ring; scheduler handles request
//	goroutines; no nested locks between server and scheduler.
//
// Invariants:
//  1. Every LSP request is admitted through the C2 lifecycle gate and the
//     scheduler (F3/F5).
//  2. didOpen/didChange/didSave/didClose update VFS and publish Snapshot (F1).
//  3. Every semantic request serves from exactly one captured Snapshot
//     (INV-SNAPSHOT-002); state transitions are never dropped for staleness
//     (PROT-SYNC-001).
//  4. The VFS is the single source of truth for open-document content.
package server

import (
	"context"
	"encoding/json"
	"errors"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/security"
	"github.com/omnilsp/omni/internal/semantic/query"
	"github.com/omnilsp/omni/internal/telemetry"
	"github.com/omnilsp/omni/internal/transport"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
	"github.com/omnilsp/omni/internal/workspace/virtual"
	"github.com/omnilsp/omni/internal/workspace/watch"
)

// State represents the server lifecycle state (goal.md §C2).
type State int

const (
	StateUninitialized State = iota
	StateInitializing
	StateRunning
	StateShuttingDown
	StateExited
)

func (st State) String() string {
	switch st {
	case StateUninitialized:
		return "uninitialized"
	case StateInitializing:
		return "initializing"
	case StateRunning:
		return "running"
	case StateShuttingDown:
		return "shutting_down"
	case StateExited:
		return "exited"
	default:
		return "unknown"
	}
}

// Server is the main LSP server.
type Server struct {
	mu sync.RWMutex
	// mutationMu serializes client state transitions and external invalidation.
	mutationMu       sync.Mutex
	backendCloseOnce sync.Once
	backendCloseErr  error
	sourceSyncStates []*sourceSyncState
	shutdownErr      error         // protected by mu; remains fatal after a later exit
	workspacePoller  *watch.Poller // protected by mu; owns the current scan baseline
	state            State
	transport        transport.Transport
	dispatcher       *jsonrpc.Dispatcher
	scheduler        *scheduler.Scheduler
	snapMgr          *snapshot.Manager
	vfs              *vfs.VFS
	virtualReg       *virtual.Registry            // §D12 source-map registry for virtual documents
	languages        map[string]languages.Backend // keyed by LanguageID
	semanticIndexes  map[string]semanticIndexBinding
	// explicitSemanticIndexes records the languages whose semantic index
	// capability was registered through RegisterSemanticIndexProvider. The
	// dirty workspace/symbol guard trusts only this set: a backend that
	// merely implements the provider/planner interfaces must keep answering
	// from the live backend when its documents are dirty.
	explicitSemanticIndexes map[string]struct{}
	overlayFacts            *goSnapshotSemanticFactsCache
	queries                 *query.Engine    // §J memo engine for semantic read paths
	diag                    *diagCoordinator // §C11/§I17 push/pull diagnostics
	positionEncoding        string           // §C4 negotiated: utf-8|utf-16|utf-32
	config                  Config
	workspaceID             identity.WorkspaceID
	idx                     *indexService
	idxReason               string
	trust                   *trust.Policy

	// syncRejects counts rejected didChange notifications (invalid range or
	// version) for observability (D6: rejection must be visible, not silent).
	syncRejects atomic.Int64

	// Rename freshness counters delimit requests that entered backend analysis
	// and were rejected because the workspace advanced before the edit was
	// returned. They make stale-result safety measurable without changing the
	// LSP rename response contract.
	renameRequestsStarted atomic.Uint64
	renameStaleRejected   atomic.Uint64

	// inflight maps JSON-RPC request IDs to scheduled requests for
	// $/cancelRequest lookup (C7). Protected by mu.
	inflight map[string]*scheduler.Request
	// completionPhaseSpans links opt-in trace samples across scheduler and
	// response-write stages. Protected by mu; nil when tracing is disabled.
	completionPhaseSpans map[string]*completionPhaseTraceSpan

	// evRing is a bounded ring of recent §B4 evidence records for the
	// omnilsp/explain API (§I25). Protected by mu.
	evRing     [64]evidenceRecord
	evRingNext int

	// metrics are the §P2 telemetry counters (§P3: low-cardinality labels only).
	metrics serverMetrics

	completionPhaseTrace *completionPhaseTraceRecorder
	// semanticResponseObserver is an optional session-recorder hook. It receives
	// request-local evidence before the matching response is written.
	semanticResponseObserver func(jsonrpc.RequestID, []identity.Evidence)

	// clientWantsDocumentChanges records the §C9 negotiation: the client
	// declared workspace.workspaceEdit.documentChanges at initialize. Set
	// once during handleInitialize, read-only afterwards.
	clientWantsDocumentChanges atomic.Bool

	// plugin lane (§O0-O5): mounted only when Config.PluginsDir is set.
	// pluginMu guards the registry maps; counters are atomic.
	// （pluginGrants 表已随审计批 3 发现 1 移除：emit 能力门的授予半边改为
	// 随 sink 闭包与被 Launch 的进程同源抵达——见 onPluginNotify。）
	pluginMu    sync.Mutex
	pluginMgr   *plugin.Manager
	pluginLangs map[string][]string // id → manifest.languages（emit 语言范围门的声明半边，审计批·合并策略来源伪造面）
	pluginProcs map[string]*plugin.Process
	// pluginDiscoverErr 记录装配期目录级发现失败的原因（pluginMu 保护）：
	// 对齐 disableIndex 的 idxReason 可观测约定——空串表示无目录级失败。
	pluginDiscoverErr string
	// pluginPending 是装配期已 Enabled、等待工作区信任策略就绪后 Launch 的
	// 插件（pluginMu 保护）。审计批 2：装配发生在 New（早于 LSP initialize），
	// 信任策略此刻通常尚未建立——「闭包延迟读取、nil 即放行」的装配期窗口
	// 已移除；initIndex 建立策略后由 launchReadyPlugins 消费。策略从未就绪
	// （索引初始化失败）时插件保持待命、绝不启动（fail-closed）。
	pluginPending []pluginPendingLaunch
	pluginOnce    sync.Once // ClosePlugins 幂等
	// pluginClosing 是关闭置位标志（pluginMu 保护，复证圈 1 审计发现 5）：
	// closePlugins 先于快照置位——launchReadyPlugins 在置位后丢弃 pending、
	// 把已 Launch 的结果按「已关闭」处理（kill + Disable 不入表），否则
	// Launch 与 closePlugins 交错时该进程不会被任何路径 shutdown/exit，
	// 进程与管道泄漏到宿主退出。
	pluginClosing bool

	// plugin counters (§P3 低基数可观测性：拒绝与丢弃可见，不静默)。
	pluginNotifies atomic.Int64 // 收到的插件通知总数
	pluginUnknown  atomic.Int64 // 非 diagnostics/emit 的通知数（暂存车道外）
	// pluginEmitRejected 是六类 emit 拒绝的混计（复证圈 3 sweep 发现 7 对账：
	// 语言域门/条数门/字节门三次扩充均未回写本注释，混计在声明面不可见）：
	// ① 能力门（onPluginNotify grant.Check，default deny 实证）；② 解析门
	// （params unmarshal/uri 空）；③ 语言域门（manifest.languages 越界 emit）；
	// ④ 条数门（maxPluginEmitDiagnostics）；⑤ 字节门（maxPluginEmitFrameBytes，
	// 驻留序列化预算）；⑥ 关闭文档的迟到 emit（injectPluginDiagnostics 的
	// vfs.Get==nil 门）。混计且无生产侧读取点（无 getter/metrics 暴露）——
	// 六类事件的发生率在计数器层面不可分辨，需拆分时按门立计数器。
	pluginEmitRejected atomic.Int64
}

// indexLadderRefreshTimeout 是 initialize 索引初始化路径上有界等待的统一
// 窗口（复证圈 1 审计发现 9 引入时护住 refreshBudgetLadder 的快照 lease；
// 复证圈 2 审计发现 8 把 refresh 改为 manifest 只读口径后不再有等待点，
// 本窗口现在护住发现 2 的最后一个跨进程等待点——NewFileStore 构造期的
// writer.lock 获取）。超时走既有 disableIndex fail-open 分支。声明在
// server.go（index.go 的新顶层声明触发 protocol manifest 重生成）。
// 非常量——测试注入更短窗口。
var indexLadderRefreshTimeout = 5 * time.Second

// wantsDocumentChanges reports whether the client negotiated the version-aware
// documentChanges WorkspaceEdit form (§C9).
func (s *Server) wantsDocumentChanges() bool { return s.clientWantsDocumentChanges.Load() }

// parseClientEditCapability inspects raw initialize capabilities for
// workspace.workspaceEdit.documentChanges == true.
func parseClientEditCapability(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var caps struct {
		Workspace struct {
			WorkspaceEdit struct {
				DocumentChanges bool `json:"documentChanges"`
			} `json:"workspaceEdit"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		return false // malformed capability payload: legacy form, fail safe
	}
	return caps.Workspace.WorkspaceEdit.DocumentChanges
}

// serverMetrics aggregates the canonical counters (§P2/§P3): every rejection,
// backend failure, and snapshot epoch must be visible, never silent.
type serverMetrics struct {
	RequestsTotal  *telemetry.Counter // omnilsp.requests.total
	RejectsTotal   *telemetry.Counter // admission/lifecycle rejections
	SyncRejects    *telemetry.Counter // didChange content rejections
	ExternalSyncs  *telemetry.Counter // workspace/didChange* external notifications
	BackendFails   *telemetry.Counter // semantic dispatch errors by language
	SnapshotEpochs *telemetry.Counter // publishes (D9)
}

func newServerMetrics() serverMetrics {
	return serverMetrics{
		RequestsTotal:  telemetry.NewCounter("omnilsp.requests.total"),
		RejectsTotal:   telemetry.NewCounter("omnilsp.rejects.total"),
		SyncRejects:    telemetry.NewCounter("omnilsp.sync.rejects"),
		ExternalSyncs:  telemetry.NewCounter("omnilsp.sync.external"),
		BackendFails:   telemetry.NewCounter("omnilsp.backend.failures"),
		SnapshotEpochs: telemetry.NewCounter("omnilsp.snapshots.epochs"),
	}
}

// evidenceRecord is one entry of the explain evidence ring.
type evidenceRecord struct {
	Method       string
	URI          string
	Ev           []identity.Evidence
	Diag         []string
	At           time.Time
	Status       identity.ResultStatus
	Completeness identity.Completeness
}

// defaultRequestDeadlines is the §F9 server-default deadline table, applied
// per §F8 cost class when Config.RequestDeadlines does not override a class.
// §F9: "Protocol adapters MAY supply deadlines; otherwise server defaults
// apply." Deadlines are a resource policy, never a semantic shortcut: they
// bound how long one request may occupy its scheduler worker, and expiry runs
// the §F9 path (scheduler cancels the work → drainResult answers the typed
// RequestCancelled response; nothing partial is published). The values scale
// with the §F8 ladder (more work ⇒ more wall clock) and sit above the soak
// harness's 30s default request budget, so under normal operation they only
// fire on work that is stuck.
//
// CostLarge is sized to the acceptance harness's 300s cold-phase envelope
// (OMNILSP_SOAK_REQUEST_BUDGET=300s, Y1-10), not to warm-query latencies:
// the first full-corpus textDocument/references request on a cold session
// pays a one-shot snapshot-overlay facts export measured at 199.3s on the
// acceptance host (x9-1-notimeout-20261029T0126Z phase trace, dispatch
// succeeded), and the previous 120s default answered RequestCancelled while
// that export was still running — failing the soak's startup on every run.
// It stays below the VeryLarge ceiling so the ladder keeps its ordering
// (TestF9_LargeDefaultCoversColdWorkspaceExportEnvelope pins both bounds).
var defaultRequestDeadlines = map[scheduler.CostClass]time.Duration{
	scheduler.CostTiny:      30 * time.Second,
	scheduler.CostSmall:     60 * time.Second,
	scheduler.CostMedium:    90 * time.Second,
	scheduler.CostLarge:     300 * time.Second,
	scheduler.CostVeryLarge: 600 * time.Second,
}

// Shutdown-drain and external-sync windows derive from these named budgets
// instead of bare integer literals at their call sites (round-16 ruling 5;
// guarded by TestNoBareSecondLiteralsInServerProductionFiles).
// defaultDrainWindow bounds how long the shutdown paths wait for in-flight
// responses to reach the wire (drainInflight); defaultExternalSyncWindow
// bounds one external source-change synchronization round trip
// (RecoverSourceChanges after an overflow fence, NotifySourceChanges in
// the regular flush). Magnitude evidence: TestDrainInflightWaitsForResponse
// Write observes a blocked response write within 20ms, and confirms 1s is
// ample to finish the write once unblocked (lifecycle_test.go); a backend
// subprocess round trip measures in single-digit milliseconds on the
// acceptance host, so 2s leaves a thousand-fold margin while staying well
// below the §F8 CostTiny default (30s). Both windows cap at the CostTiny
// request deadline when one is configured — see drainWindow and
// externalSyncWindow.
const (
	defaultDrainWindow        = 2 * time.Second
	defaultExternalSyncWindow = 2 * time.Second
)

// Config holds server configuration.
type Config struct {
	Scheduler            scheduler.Config
	IndexDir             string
	IndexDiskBudgetBytes int64
	// WatchInterval > 0 enables the §D14 workspace poller (external file
	// changes are rescanned on this cadence). Zero disables it: editors that
	// drive didChangeWatchedFiles themselves need no second opinion.
	WatchInterval time.Duration
	// RequestDeadlines maps each §F8 cost class to the default wall-clock
	// deadline applied to scheduled requests of that class (§F9: protocol
	// adapters MAY supply deadlines; otherwise server defaults apply —
	// DefaultConfig fills this table). The deadline bounds the request's
	// whole scheduled lifetime, queue wait included, so a stuck backend
	// cannot pin a worker forever; on expiry the scheduler stops the work and
	// the client receives the typed RequestCancelled response. A zero or
	// missing entry means the class runs without a server-imposed deadline.
	RequestDeadlines map[scheduler.CostClass]time.Duration
	// ShutdownDrainWindow bounds how long the shutdown paths wait for
	// in-flight responses to reach the wire (drainInflight; handleShutdown
	// and Run's three transport-exit paths). Zero derives the window from
	// named budgets — min(defaultDrainWindow, RequestDeadlines[CostTiny]) —
	// the same zero-value-means-derived convention WatchInterval follows.
	ShutdownDrainWindow time.Duration
	// ExternalSyncWindow bounds one external source-change synchronization
	// round trip (RecoverSourceChanges after an overflow fence,
	// NotifySourceChanges in the regular flush). Zero derives
	// min(defaultExternalSyncWindow, RequestDeadlines[CostTiny]).
	ExternalSyncWindow time.Duration
	// PluginsDir enables the process-out-of-plugin lane (goal.md §O0-O5): the
	// server mounts every verified plugin found under this directory at New
	// time (Discover → Install → Enable → Launch) and routes its
	// diagnostics/emit notifications through the capability gate into the
	// diagnostics coordinator. Zero value disables the lane entirely — no
	// discovery, no subprocess, no behavior change (X10: default off).
	PluginsDir string
}

// DefaultConfig returns default server configuration.
func DefaultConfig() Config {
	deadlines := make(map[scheduler.CostClass]time.Duration, len(defaultRequestDeadlines))
	for class, d := range defaultRequestDeadlines {
		deadlines[class] = d
	}
	return Config{
		Scheduler:            scheduler.DefaultConfig(),
		IndexDiskBudgetBytes: 256 << 20,
		RequestDeadlines:     deadlines,
	}
}

// requestDeadline returns the §F9 default deadline for a cost class; zero
// means the class runs without a server-imposed deadline.
func (s *Server) requestDeadline(class scheduler.CostClass) time.Duration {
	return s.config.RequestDeadlines[class]
}

// drainWindow returns the wall-clock budget for the shutdown drain paths: an
// explicit Config.ShutdownDrainWindow wins; otherwise the window derives from
// named budgets — min(defaultDrainWindow, the §F9 CostTiny deadline). A zero
// CostTiny deadline means "no server-imposed deadline" (requestDeadline), so
// the unbounded case falls back to defaultDrainWindow rather than zeroing the
// drain.
func (s *Server) drainWindow() time.Duration {
	if v := s.config.ShutdownDrainWindow; v > 0 {
		return v
	}
	if d := s.requestDeadline(scheduler.CostTiny); d > 0 {
		return min(defaultDrainWindow, d)
	}
	return defaultDrainWindow
}

// externalSyncWindow is drainWindow's twin for external source-change
// synchronization round trips: an explicit Config.ExternalSyncWindow wins;
// otherwise min(defaultExternalSyncWindow, the §F9 CostTiny deadline), with
// the same unbounded-deadline fallback.
func (s *Server) externalSyncWindow() time.Duration {
	if v := s.config.ExternalSyncWindow; v > 0 {
		return v
	}
	if d := s.requestDeadline(scheduler.CostTiny); d > 0 {
		return min(defaultExternalSyncWindow, d)
	}
	return defaultExternalSyncWindow
}

// New creates a new LSP server.
func New(cfg Config) *Server {
	s := &Server{
		state:                   StateUninitialized,
		config:                  cfg,
		dispatcher:              jsonrpc.NewDispatcher(),
		snapMgr:                 snapshot.NewManager(),
		vfs:                     vfs.New(),
		virtualReg:              virtual.New(),
		languages:               make(map[string]languages.Backend),
		semanticIndexes:         make(map[string]semanticIndexBinding),
		explicitSemanticIndexes: make(map[string]struct{}),
		overlayFacts:            newGoSnapshotSemanticFactsCache(),
		inflight:                make(map[string]*scheduler.Request),
		metrics:                 newServerMetrics(),
		queries:                 query.NewEngine(0),
		completionPhaseTrace:    newCompletionPhaseTraceFromEnv(),
	}
	s.diag = newDiagCoordinator(s)
	s.positionEncoding = "utf-16" // LSP baseline default until negotiated
	s.scheduler = scheduler.New(cfg.Scheduler)
	s.registerHandlers()
	if cfg.PluginsDir != "" {
		s.mountPlugins(cfg.PluginsDir)
	}
	return s
}

// pluginEmitParams 是插件 diagnostics/emit 通知的 params 形状（插件协议 v1）。
// 插件不可携带 source 字段：来源标记由宿主强制生成（来源治理）。
type pluginEmitParams struct {
	URI         string           `json:"uri"`
	Diagnostics []pluginEmitDiag `json:"diagnostics"`
}

// maxPluginEmitDiagnostics 是单次插件 emit 的诊断条数预算（复证圈 2 审计
// 发现 3）：解析后的诊断集整集驻留 pluginItems，FIFO 只限 uri 数不限每 uri
// 内容——每 emit 超过该条数即整帧拒绝（可见计数，不部分采纳）。
const maxPluginEmitDiagnostics = 1024

// maxPluginEmitFrameBytes 是单次插件 emit 驻留集的序列化字节预算（复证圈 1
// sweep 发现 1）：条数上限不约束单条体积，diagCacheLimit=512 的 FIFO 只按
// uri 计数——驻留总量 = 打开文档数 × 插件数 × 单帧解析后结构，GiB 量级可达
// 且此前无字节记账。预算按注入前 projectDiagnostics 的序列化长度设硬上限
// （记账口径与真正上线的 payload 一致），超限整帧拒绝并计入
// pluginEmitRejected——与条数门同待遇，不部分采纳。取值与插件通道单帧上限
// MaxPluginFrameSize（4MiB）同锚：协议上不存在比它更大的合法 emit 帧，预算
// 拦下的是转投影展开后的极端驻留（消息体再转义/结构展开放大），常规 emit
// 不受影响。任务书点名的默认行为收紧（非 X10 默认关项）。代价：emit 路径
// 每帧多一次 marshal（≤1024 条，发布侧的去抖 marshal 不变）。
const maxPluginEmitFrameBytes = 4 << 20

// pluginMountBudget 是单次插件装配（Discover + 全量信任链复核）的整体预算
// （复证圈 2 审计发现 6）：装配同步运行在 TCP accept 关键路径上且每个会话
// 重复，无界 sha256 复核让 accept 与首字节延迟无界；超时落入 pluginDiscoverErr
// 可观测路径。取 c7a1ef6 toolchain pinning 加界先例（probe 30s / 整体 2m）
// 的整体档。非常量——测试注入更短窗口。
var pluginMountBudget = 2 * time.Minute

type pluginEmitDiag struct {
	StartLine uint32 `json:"startLine"`
	StartChar uint32 `json:"startChar"`
	EndLine   uint32 `json:"endLine"`
	EndChar   uint32 `json:"endChar"`
	Severity  int    `json:"severity"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// pluginPendingLaunch 是一条待 Launch 的装配结果：discovery 提供目录来源与
// 有效 manifest（声明能力经装配点授予策略过滤后的副本，原始声明留在 Manager
// 注册表与磁盘清单），grant 是 EnableWithPolicy 建立的授权表。
type pluginPendingLaunch struct {
	discovery plugin.Discovery
	grant     plugin.Grant
}

// pluginCapsAllow 解析 OMNILSP_PLUGIN_CAPS_ALLOW（逗号分隔能力名）：装配点
// 高权能力第二道门的显式 opt-in 面（admin 策略/用户信任输入）。空/未设 =
// 无 opt-in（高权默认拒绝）。低权与未知能力名在此无效果——低权本就按声明
// 授予，未知名会被 DefaultGrantPolicy 静默忽略（装配面不做二次校验）。
func pluginCapsAllow() map[plugin.Capability]bool {
	raw := os.Getenv("OMNILSP_PLUGIN_CAPS_ALLOW")
	if raw == "" {
		return nil
	}
	allow := make(map[plugin.Capability]bool)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allow[plugin.Capability(name)] = true
		}
	}
	return allow
}

// mountPlugins 是进程内插件装配点（§O0-O5 落差1）：PluginsDir 非空时执行
// Discover → Install → EnableWithPolicy（高权能力第二道门，审计批·注册变更
// 越权覆盖面）。单插件失败不中断整体（与 Discover 同哲学）：
// VerifyErr 目录被 Install 拒绝（信任链失败不 spawn）、Enable 失败（崩溃隔离
// 态）只记账不重试。
//
// Launch 不发生在装配期（审计批 2）：工作区信任策略（trust.Policy）由
// initialize 的 initIndex 建立，装配期通常尚未就绪——旧实现「闭包延迟读取、
// nil 即放行、后续 Enable 路径补足」的时序豁免已撤。插件停留 enabled 待命，
// 由 launchReadyPlugins 在策略就绪后推进 Launch；工作区信任断言（§N1/N3
// plugin.load）下沉为 Launch 的必经前置（plugin.Launch 的 trust 参数，nil 即拒），
// 绕过 Enable 直达 Launch 的路径同样过门。
func (s *Server) mountPlugins(dir string) {
	mgr := plugin.NewManager()

	s.pluginMu.Lock()
	if s.pluginMgr != nil { // 幂等：每个 Server 实例至多装配一次
		s.pluginMu.Unlock()
		return
	}
	s.pluginMgr = mgr
	s.pluginLangs = make(map[string][]string)
	s.pluginProcs = make(map[string]*plugin.Process)
	s.pluginMu.Unlock()

	// 装配预算（复证圈 2 审计发现 6）：装配同步运行在 TCP accept 关键路径上
	// 且每个会话重复（server.New → mountPlugins）——Discover + 全文件 sha256
	// 信任链复核无 deadline 时，大插件目录让 accept 与首字节延迟无界。超时以
	// 目录级失败落入 pluginDiscoverErr 可观测路径（与目录级失败同一约定）。
	mountCtx, cancel := context.WithTimeout(context.Background(), pluginMountBudget)
	defer cancel()
	discoveries, err := mgr.DiscoverContext(mountCtx, dir)
	if err != nil {
		// 目录级失败对齐 disableIndex 的可观测约定（index.go）：状态字段
		// 记录原因 + stderr 一条——「不可用 PluginsDir」不得伪装成「无插件」。
		s.pluginMu.Lock()
		s.pluginDiscoverErr = err.Error()
		s.pluginMu.Unlock()
		security.FprintfRedacted(os.Stderr, "omnilsp: plugin discovery failed: %v\n", err)
		return
	}
	seen := make(map[string]struct{}, len(discoveries))
	for _, d := range discoveries {
		id := d.Manifest.ID
		if _, dup := seen[id]; dup {
			// manifest.ID 唯一性（复证圈 2 审计发现 1）：重复 ID 的发现结果
			// 可见拒绝——同 ID 的两个插件目录互相顶替会破坏有界拆除（宿主
			// 注册表同键覆盖使先 Launch 的进程失去登记句柄）、崩溃隔离
			// （RecordCrash 按 ID 连坐）与 emit 来源治理。Install 侧的
			// ErrDuplicatePluginID 是第二道闸。
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s 重复 manifest.ID（目录 %s）拒绝装配\n", id, d.Dir)
			continue
		}
		seen[id] = struct{}{}
		if err := mgr.Install(d); err != nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s 未通过信任链，拒绝装配: %v\n", id, err)
			continue
		}
		// 装配点授予第二道门（审计批·注册变更越权覆盖面）：声明能力先过授予
		// 策略再入 Grant——三项高权能力（process.execute/filesystem.write/
		// network.access）默认不授予，仅 OMNILSP_PLUGIN_CAPS_ALLOW（逗号分隔，
		// admin 策略/用户显式 opt-in）列名者放行；低权能力照常按声明授予。
		// 「Enable 即按声明授予」的旧约定使凡进入 PluginsDir 且过 checksum 的
		// 插件即自授全部声明能力，default deny 对已声明项没有第二守门人。
		policy := plugin.DefaultGrantPolicy(pluginCapsAllow())
		grant, err := mgr.EnableWithPolicy(id, policy)
		if err != nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s 启用被拒: %v\n", id, err)
			continue
		}
		// 有效声明与授予表由同一策略产出（plugin.ApplyGrantPolicy）：Launch 前置
		// 门（声明 ⊆ 授予）按构造成立——高权能力被剔除时同步从 pending 携带的
		// manifest 副本中移除，插件进程侧能力表随之降级，而 Manager 注册表与
		// CLI 展示仍保留原始声明。被剔除的声明能力落 stderr 可见记账；全部声明
		// 被剔除的插件仍装配（空能力表 = 纯 inert，所有 Call/emit 门拒绝）。
		if eff := plugin.ApplyGrantPolicy(d.Manifest.Capabilities, policy); len(eff) != len(d.Manifest.Capabilities) {
			granted := make(map[plugin.Capability]bool, len(eff))
			for _, c := range eff {
				granted[c] = true
			}
			var removed []string
			for _, c := range d.Manifest.Capabilities {
				if !granted[c] {
					removed = append(removed, string(c))
				}
			}
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s 声明的高权能力未获显式授权（OMNILSP_PLUGIN_CAPS_ALLOW），已从授予表剔除: %s\n", id, strings.Join(removed, ","))
			d.Manifest.Capabilities = eff
		}
		s.pluginMu.Lock()
		s.pluginLangs[id] = d.Manifest.Languages // 语言范围门的声明半边（语言不经能力策略过滤，原始声明即有效声明）
		s.pluginPending = append(s.pluginPending, pluginPendingLaunch{discovery: d, grant: grant})
		s.pluginMu.Unlock()
	}
}

// launchReadyPlugins 把装配期待命的插件推进到 Launch：由 initIndex 在工作区
// 信任策略建立后调用（index.go）。信任裁决只有一处——plugin.Launch 的必经
// 前置 trust 回调（现读 trust.Policy 过 plugin.load 门，nil 策略即拒），拒绝
// 与失败都落 stderr 并 Disable（死插件不持有 Enabled 态授权）；Launch 失败
// 不计入 RecordCrash——executor 的 §O4 语义把握手期失败排除在崩溃循环之外。
// 幂等：pending 队列消费即清空，无 Manager 或空队列零动作。
func (s *Server) launchReadyPlugins() {
	s.pluginMu.Lock()
	pending := s.pluginPending
	s.pluginPending = nil
	mgr := s.pluginMgr
	closing := s.pluginClosing // 复证圈 1 审计发现 5：关闭置位后不再起飞
	s.pluginMu.Unlock()
	if mgr == nil || len(pending) == 0 {
		return
	}
	for _, e := range pending {
		id := e.discovery.Manifest.ID
		if closing {
			// 关闭置位后 pending 直接 Disable 丢弃，不 Launch（复证圈 1
			// 审计发现 5：ClosePlugins 置位后的起飞尝试按已关闭处理）。
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s launch 丢弃（服务器关闭中）\n", id)
			_ = mgr.Disable(id)
			continue
		}
		pluginID := id
		// 审计批 3 发现 1：grant 随 sink 闭包与被 Launch 的进程同源抵达——
		// 通知门的能力判定用「该进程被 Launch 时所持的同一张授予表」，不经
		// 任何宿主侧登记表转手。旧实现的 pluginGrants 登记在 Launch 返回 +
		// MarkRunning 之后才落表，sink 却已在 readLoop/pump 启动前接活：插件
		// 收到 plugin/hello 后立即 emit 的通知在查表门被拒（pluginEmitRejected
		// ），「注册窗口从协议时序上整体移除」的旧注释与实际不符——窗口内
		// 通知到不了诊断车道，只是丢弃科目从 DroppedNotifies 换名。闭包捕获
		// 使「已接 sink 但未登记」态在存活路径上不可达，窗口按构造闭合；
		// 零值 Grant 全 deny（plugin.DefaultGrants），fail-closed 语义不变。
		proc, err := plugin.LaunchWithSink(context.Background(), e.discovery.Dir, e.discovery.Manifest, e.grant, func(werr error) {
			s.pluginOnExit(id, werr)
		}, func() error {
			if _, policy, _ := s.indexState(); policy != nil {
				return policy.Gate(trust.CapPluginLoad)
			}
			return fmt.Errorf("plugin: 工作区信任策略尚未建立")
		}, func(method string, params json.RawMessage) {
			s.onPluginNotify(pluginID, e.grant, method, params)
		})
		if err != nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s launch 被拒: %v\n", id, err)
			_ = mgr.Disable(id)
			continue
		}
		mgr.MarkRunning(id)
		// 订阅接线（第 18 轮治理开口 4 + 审计批 3 发现 1）：sink 经
		// LaunchWithSink 在 readLoop/pump 启动前注册，且 grant 随同一闭包抵达
		// 通知门（见上方 LaunchWithSink 调用点注记）——握手期/启动期到达的
		// 通知从协议时序上整体可投递，无登记窗口、无 DroppedNotifies 丢弃路径
		// 的旧窗口残留（旧实现该窗口的通知被 grant 查表门拒为
		// pluginEmitRejected，仅对「无订阅者」与通道饱和保留有界有损丢弃）。
		s.pluginMu.Lock()
		if s.pluginClosing {
			// 复证圈 1 审计发现 5：Launch 与 ClosePlugins 交错——Launch 期间
			// 关闭已置位，快照错过该进程（旧实现此刻才入表，进程永不被
			// shutdown/exit，泄漏到宿主退出）。已 Launch 的结果按已关闭处理：
			// kill + Disable，不入表。
			s.pluginMu.Unlock()
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s launch 完成于关闭之后，按已关闭拆除\n", id)
			if kerr := proc.Kill(); kerr != nil {
				security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s kill: %v\n", id, kerr)
			}
			_ = mgr.Disable(id)
			continue
		}
		s.pluginProcs[id] = proc
		s.pluginMu.Unlock()
	}
}

// onPluginNotify 是插件→宿主通知的宿主半边（pump 线程调用）：统计、能力门
// （grant.Check——default deny 的 emit 实证点）、解析、语言范围门
// （manifest.languages 与目标文档语言比对，审计批·合并策略来源伪造面），
// 最后把诊断集带宿主强制来源标记汇入 diagnostics 协调器（去抖路径，不直连推送）。
//
// grant 形参是审计批 3 发现 1 的修复形态：授予表由 LaunchWithSink 的 sink
// 闭包随进程同源传入（该进程被 Launch 时所持的同一张授予表），不经宿主侧
// 登记表转手——旧实现查 pluginGrants 表，登记滞后于 sink 使握手期 emit 在
// 「未登记」分支被拒（丢弃科目从 DroppedNotifies 换名，窗口未消）。零值
// Grant 全 deny（plugin.DefaultGrants），fail-closed 语义不变。
func (s *Server) onPluginNotify(pluginID string, grant plugin.Grant, method string, params json.RawMessage) {
	s.pluginNotifies.Add(1)
	if method != "diagnostics/emit" {
		s.pluginUnknown.Add(1) // 车道外方法：可见丢弃（最小切片只放行单能力）
		return
	}
	// 能力门（§O2）：无 diagnostics.emit 授予的 emit 在此拒绝——default deny。
	if err := grant.Check(plugin.CapDiagnosticsEmit); err != nil {
		s.pluginEmitRejected.Add(1)
		return
	}
	var ep pluginEmitParams
	if err := json.Unmarshal(params, &ep); err != nil || ep.URI == "" {
		s.pluginEmitRejected.Add(1)
		return
	}
	// 复证圈 7 sweep 发现 1：语言范围门与注入必须用同一 uri 形态——旧实现
	// 把插件可控的原始 ep.URI 原样传给 pluginWithinLanguageScope，而
	// injectPluginDiagnostics 注入前才 canonicalDocumentURI：percent-encoding
	// 变体（"prog.%70%79"）在原始文本上后缀不命中 → 语言不可判定 fail-open
	// 放行 → 注入命中 canonical 键，跨语言诊断上线。此处先行 canonical 化，
	// 两个消费点（语言门 vfs/后缀分支、injectPluginDiagnostics）天然同形态。
	ep.URI = canonicalDocumentURI(ep.URI)
	// 语言范围门（审计批·合并策略来源伪造面）：manifest.languages（§O3 必填）
	// 此前在 emit 路径零执行——声明 languages:[go] 的插件可对任意打开文档
	// emit 诊断（onPluginNotify 只查授予、预算与打开态）。越界 emit 与能力门
	// 同待遇：拒绝并计入 pluginEmitRejected 可见计数。
	if !s.pluginWithinLanguageScope(pluginID, ep.URI) {
		s.pluginEmitRejected.Add(1)
		return
	}
	// 条数预算（复证圈 2 审计发现 3）：单帧有 executor 的 MaxPluginFrameSize
	// （4MiB）兜底，但解析后的诊断集整集驻留 pluginItems——FIFO 只限 uri 数
	// （diagCacheLimit=512）不限每 uri 内容，无预算接车。超限整帧拒绝并计入
	// 可见拒绝计数（不部分采纳——整帧拒绝与 LSP 全量报告语义一致）。
	if len(ep.Diagnostics) > maxPluginEmitDiagnostics {
		s.pluginEmitRejected.Add(1)
		return
	}
	items := make([]languages.Diagnostic, 0, len(ep.Diagnostics))
	for _, dg := range ep.Diagnostics {
		items = append(items, languages.Diagnostic{
			StartLine: dg.StartLine, StartChar: dg.StartChar,
			EndLine: dg.EndLine, EndChar: dg.EndChar,
			Severity: dg.Severity, Code: dg.Code, Message: dg.Message,
			// 来源治理：宿主强制覆盖——插件无法伪装宿主或后端来源。
			Source: "plugin:" + pluginID,
		})
	}
	// 字节预算（复证圈 1 sweep 发现 1，见 maxPluginEmitFrameBytes）：条数门
	// 之后的第二道驻留面闸门——按投影序列化长度整帧拒绝，超限计入
	// pluginEmitRejected。marshal 失败（projectDiagnostics 输出的纯 JSON
	// 安全类型上不可达）按 fail-closed 处理：无法记账的集不驻留。
	if wire, merr := json.Marshal(projectDiagnostics(items)); merr != nil || len(wire) > maxPluginEmitFrameBytes {
		s.pluginEmitRejected.Add(1)
		return
	}
	s.diag.injectPluginDiagnostics(pluginID, ep.URI, items)
}

// pluginWithinLanguageScope 报告插件对 uri 的 emit 是否落在其 manifest 声明
// 的语言范围内（审计批·合并策略来源伪造面）。目标文档语言的判定来源：打开
// 文档的 languageId（didOpen 客户端声明，VFS 权威），辅以注册后端扩展名匹配
// 的语言 ID（languageId 为空时的后缀兜底，对齐 resolveBackend 的匹配语义）；
// 与声明语言求交（大小写不敏感）。语言不可判定（文档未打开/无 languageId
// 且无后端命中）时保持既有放行语义——本门针对「声明 languages:[go] 却对可
// 判定语言文档越界 emit」的伪造面，未打开文档由 injectPluginDiagnostics 的
// 打开态门拒绝，语言未知时的通用投放治理留待 §O 后续轮次。
func (s *Server) pluginWithinLanguageScope(pluginID, uri string) bool {
	s.pluginMu.Lock()
	declaredList := s.pluginLangs[pluginID]
	s.pluginMu.Unlock()
	if len(declaredList) == 0 {
		// 无声明信息不设门（防御半边：§O3 校验保证 manifest.languages 非空，
		// 仅非常规装配路径可能缺席）。
		return true
	}
	declared := make(map[string]bool, len(declaredList))
	for _, l := range declaredList {
		declared[strings.ToLower(strings.TrimSpace(l))] = true
	}
	var resolved []string
	if st := s.vfs.Get(uri); st != nil && st.LanguageID != "" {
		resolved = append(resolved, strings.ToLower(strings.TrimSpace(st.LanguageID)))
	}
	s.mu.RLock()
	for langID, be := range s.languages {
		for _, ext := range be.FileExtensions() {
			if strings.HasSuffix(strings.ToLower(uri), strings.ToLower(ext)) {
				resolved = append(resolved, strings.ToLower(langID))
				break
			}
		}
	}
	s.mu.RUnlock()
	if len(resolved) == 0 {
		return true
	}
	for _, l := range resolved {
		if declared[l] {
			return true
		}
	}
	return false
}

// pluginOnExit 接入 Manager.RecordCrash（§O4 崩溃循环遏制）：进程异常退出
// （err 非 nil）计一次连续崩溃，达 MaxConsecutivePluginCrashes 即隔离；
// 干净退出（shutdown→exit 握手完成，err==nil）不计——那不是崩溃。
func (s *Server) pluginOnExit(id string, err error) {
	s.pluginMu.Lock()
	delete(s.pluginProcs, id)
	mgr := s.pluginMgr
	s.pluginMu.Unlock()
	if mgr == nil || err == nil {
		return
	}
	mgr.RecordCrash(id)
}

// ClosePlugins 优雅关闭全部插件进程（Run 收尾与宿主退出路径共用）：按 LSP
// 语义 shutdown → exit。Shutdown 失败（进程已死，或存活但挂起应答）不再只
// 跳过——统一补一次有界 kill（复证圈 1 审计发现 7），有界拆除覆盖全部退出
// 路径。幂等。
func (s *Server) ClosePlugins() {
	s.pluginOnce.Do(s.closePlugins)
}

func (s *Server) closePlugins() {
	s.pluginMu.Lock()
	s.pluginClosing = true // 复证圈 1 审计发现 5：先置位再快照，堵住 Launch 交错窗口
	procs := s.pluginProcs
	s.pluginProcs = make(map[string]*plugin.Process)
	s.pluginMu.Unlock()
	for id, p := range procs {
		if err := p.Shutdown(); err != nil {
			// 复证圈 1 审计发现 7：Shutdown 失败的旧实现只 continue——而 Exit
			// 是唯一带 kill 的路径，于是「存活但挂起应答」（Call 超时，而非
			// 进程已死）的插件永不终止、OnExit/RecordCrash 不闭合，进程与两根
			// stdio 管道泄漏为孤儿（这正是 32f5afd 为 Exit 腿修掉的同级故障
			// 在 Shutdown 腿重开）。统一补一次有界 kill：已死进程上 kill 是
			// 无害 no-op，存活挂起的进程被强制终止并经 Wait→OnExit 收割。
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s shutdown: %v（有界拆除：强制终止）\n", id, err)
			if kerr := p.Kill(); kerr != nil {
				security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s kill: %v\n", id, kerr)
			}
			continue
		}
		if err := p.Exit(); err != nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: plugin %s exit: %v\n", id, err)
		}
	}
}

// RegisterBackend registers a language backend for the given language ID.
// VFS exposes the virtual filesystem for tooling and tests.
func (s *Server) VFS() *vfs.VFS { return s.vfs }

// Dispatcher exposes the JSON-RPC dispatcher for tooling and tests.
func (s *Server) Dispatcher() *jsonrpc.Dispatcher { return s.dispatcher }

type diagnosticUpdateSource interface {
	SetDiagnosticsUpdateHandler(func(uri string))
}

func (s *Server) RegisterBackend(langID string, backend languages.Backend) {
	s.mu.Lock()
	s.languages[langID] = backend
	if provider, ok := backend.(languages.SemanticIndexProvider); ok {
		if planner, ok := backend.(languages.SemanticIndexRequestBuilder); ok {
			// Backend aliases (for example "c" for a canonical "cpp" backend)
			// must not create a second semantic job with a different language key.
			// Request builders derive scope language from their canonical backend
			// identity, so bind the capability under that same identity.
			semanticLanguage := backend.LanguageID()
			if semanticLanguage == "" {
				semanticLanguage = langID
			}
			s.semanticIndexes[semanticLanguage] = semanticIndexBinding{provider: provider, planner: planner}
		}
	}
	s.mu.Unlock()
	if source, ok := backend.(diagnosticUpdateSource); ok {
		source.SetDiagnosticsUpdateHandler(s.diag.diagnosticsUpdated)
	}
}

// RegisterSemanticIndexProvider binds an optional external semantic exporter
// and request planner to a language key. It is used by language stacks where
// the exporter is separate from the frozen Backend implementation, and by
// backends whose own provider/planner capability must be trusted explicitly
// for the dirty workspace/symbol guard.
func (s *Server) RegisterSemanticIndexProvider(langID string, provider languages.SemanticIndexProvider, planner languages.SemanticIndexRequestBuilder) {
	if langID == "" || provider == nil || planner == nil {
		return
	}
	s.mu.Lock()
	s.semanticIndexes[langID] = semanticIndexBinding{provider: provider, planner: planner}
	s.explicitSemanticIndexes[langID] = struct{}{}
	s.mu.Unlock()
}

// hasExplicitSemanticIndex reports whether the language's semantic index
// capability was registered explicitly through RegisterSemanticIndexProvider.
// The dirty workspace/symbol guard consults this set rather than the backend
// interfaces: only an explicit registration may fail a dirty request closed,
// so a backend whose planner cannot prove the snapshot (for example a ccls
// bridge without compile_commands.json) keeps falling back to the live
// backend instead.
func (s *Server) hasExplicitSemanticIndex(language string) bool {
	if language == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.explicitSemanticIndexes[language]
	return ok
}

// semanticIndexFamily normalizes a language key to its semantic-index family:
// the canonical language under which one exporter capability is enumerated.
// Backend aliases and editor dialect keys share the capability of their
// canonical backend identity — the ccls bridge serves .c/.h documents under
// the cpp family (its planner derives every scope from the canonical cpp/ccls
// identity), and the TypeScript exporter serves the javascript,
// typescriptreact, and javascriptreact dialects under the typescript family.
// Mirrors the family grouping in semanticPlanningStillMatches.
func semanticIndexFamily(language string) string {
	switch language {
	case "c":
		return "cpp"
	case "javascript", "typescriptreact", "javascriptreact":
		return "typescript"
	default:
		return language
	}
}

// Run runs the server with the given transport. Blocks until shutdown.
// All incoming messages are admitted through the lifecycle gate (C2) and the
// scheduler (F3/F5). The Running state is entered only via the initialize
// handshake, never implicitly.
func (s *Server) Run(ctx context.Context, t transport.Transport) (runErr error) {
	s.mu.Lock()
	s.transport = t
	s.mu.Unlock()

	s.scheduler.Start(ctx)
	var watcher *watch.Poller
	defer func() {
		if watcher != nil {
			watcher.CloseAndWait()
		}
		if err := s.CloseSession(); err != nil {
			runErr = stderrors.Join(runErr, err)
		}
		if err := s.completionPhaseTrace.close(); err != nil {
			runErr = stderrors.Join(runErr, err)
		}
	}()

	for {
		// C2/Q3: transport termination must surface its cause. Done alone is
		// not consulted here — Read reports either the stream error (e.g.
		// truncated frame, §S13) or a clean EOF; skipping straight to nil
		// would swallow the difference.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := t.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// LSP exit closes the transport deliberately. In that one state,
			// Read may return io.ErrClosedPipe instead of EOF; this is the
			// successful completion of the lifecycle handshake, not a transport
			// fault. Unexpected read failures still surface below.
			if s.State() == StateExited {
				return s.drainInflight(s.drainWindow())
			}
			// Clean EOF is a normal stream end (peer closed after finishing,
			// replay drain); any other error is a transport fault that must
			// surface — a truncated frame must never masquerade as done (S13).
			if stderrors.Is(err, io.EOF) {
				return s.drainInflight(s.drainWindow())
			}
			return stderrors.Join(err, s.drainInflight(s.drainWindow()))
		}

		if msg == nil {
			continue
		}

		if msg.IsResponse() {
			continue
		}

		// Route through scheduler for admission control and prioritization.
		s.scheduleMessage(ctx, msg)
		// The ordinary LSP root is only available after initialize. The
		// callback shares the document writer, including while Read blocks.
		if watcher == nil && s.config.WatchInterval > 0 && s.State() == StateRunning {
			if root := s.workspaceRoot(); root != "" {
				watcher = watch.New(root, s.config.WatchInterval, func(events []watch.Event) {
					if len(events) == 0 {
						return
					}
					s.mutationMu.Lock()
					defer s.mutationMu.Unlock()
					if s.State() != StateRunning {
						return
					}
					changes := make([]languages.SourceChange, 0, len(events))
					for _, event := range events {
						changes = append(changes, languages.SourceChange{
							URI: workspaceuri.FromPath(event.Path).Canonical(), Kind: languages.SourceChangeKind(event.Kind + 1),
						})
					}
					if err := s.applyExternalSourceChanges(ctx, changes); err != nil {
						s.recordEvidence(ctx, "workspace/poll", "", identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
					}
				})
				if idx, _, _ := s.indexState(); idx != nil && idx.dir != "" {
					if err := watcher.SetExcludedRoots(idx.dir); err != nil {
						watcher.CloseAndWait()
						return fmt.Errorf("configure workspace watcher index exclusion: %w", err)
					}
				}
				watcher.SetErrorHandler(func(err error) {
					s.mutationMu.Lock()
					defer s.mutationMu.Unlock()
					if s.State() != StateRunning {
						return
					}
					if err != nil {
						s.recordEvidence(context.Background(), "workspace/poll", "", identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
					} else {
						s.recordEvidence(context.Background(), "workspace/poll", "", identity.ResultExact, identity.Complete, nil, []string{"complete scan recovered"})
					}
				})
				s.mu.Lock()
				s.workspacePoller = watcher
				s.mu.Unlock()
				watcher.Start()
			}
		}
	}
}

// CloseSession runs the session teardown tail shared by Run's defer chain and
// any path that serves without Run (audit batch 2, finding 2: the MCP
// projection never entered Run, so the backends leg — the only teardown point
// for the supervised nested language servers — never executed there).
//
// Order is the historical Run defer order: scheduler shutdown, plugin graceful
// teardown (shutdown→exit + bounded kill), backend reaping (closeBackends,
// which closes the diagnostics coordinator first), then the shutdown-request
// error and a final diag.Close. Every step is idempotent (pluginOnce /
// backendCloseOnce / coordinator close), so calling it from two paths — or
// twice — is safe. The error joins closeBackends' reap failures with the
// pending shutdown-request error, mirroring what Run used to return.
func (s *Server) CloseSession() error {
	s.scheduler.Shutdown()
	s.ClosePlugins()
	err := s.closeBackends()
	s.mu.RLock()
	err = stderrors.Join(err, s.shutdownErr)
	s.mu.RUnlock()
	s.diag.Close()
	return err
}

// closeBackends reaps each unique child service once. Shutdown calls it before
// acknowledging the request so exit does not race the client's process cleanup.
func (s *Server) closeBackends() error {
	s.backendCloseOnce.Do(func() {
		s.diag.Close()
		backends := s.workspaceBackends()
		errorsByBackend := make([]error, len(backends))
		var workers sync.WaitGroup
		limit := make(chan struct{}, 8)
		for i, be := range backends {
			workers.Add(1)
			go func(i int, be languages.Backend) {
				defer workers.Done()
				limit <- struct{}{}
				defer func() { <-limit }()
				if err := be.Close(); err != nil {
					errorsByBackend[i] = fmt.Errorf("close %s backend: %w", be.LanguageID(), err)
				}
			}(i, be)
		}
		workers.Wait()
		s.backendCloseErr = stderrors.Join(errorsByBackend...)
	})
	return s.backendCloseErr
}

// inlineMethods are handled directly on the read-loop goroutine instead of
// the scheduler pool (F1 single-writer). State transitions must be applied
// strictly in arrival order and never dropped: routing them through the
// worker pool let concurrent workers apply didChanges out of order, and the
// VFS version-regression guard silently dropped the losers (PROT-SYNC-001).
// They are also cheap (string splices, state flips), so inlining costs the
// loop nothing while making cancel/lifecycle handling immediate (A3).
var inlineMethods = map[string]bool{
	"initialize":                      true,
	"initialized":                     true,
	"shutdown":                        true,
	"exit":                            true,
	"$/cancelRequest":                 true,
	"textDocument/didOpen":            true,
	"textDocument/didChange":          true,
	"textDocument/didSave":            true,
	"textDocument/didClose":           true,
	"workspace/didChangeWatchedFiles": true,
	"workspace/didCreateFiles":        true,
	"workspace/didRenameFiles":        true,
	"workspace/didDeleteFiles":        true,
}

// scheduleMessage admits the message: lifecycle/state-transition methods run
// inline on the caller goroutine (the read loop — a natural single writer);
// everything else goes through the scheduler for admission control and
// prioritization (F3/F5). The scheduler is therefore a query engine only.
func (s *Server) scheduleMessage(parentCtx context.Context, msg *jsonrpc.Message) {
	s.metrics.RequestsTotal.Inc(1)
	if inlineMethods[msg.Method] {
		s.dispatchInline(msg)
		return
	}

	if !s.lifecycleAllows(msg.Method) {
		s.respondLifecycleError(msg)
		return
	}
	priority := s.classifyPriority(msg)
	phaseSpan := s.beginScheduledPhaseTrace(msg)

	// Track in-flight requests for $/cancelRequest mapping (C7).
	var reqID string
	if msg.ID != nil {
		reqID = requestIDKey(*msg.ID)
	}

	snap := s.snapMgr.Current()
	// Preserve the client-visible wire ID: error responses must echo it, not
	// the internal RequestID string (which is not a JSON-RPC id).
	var origID *scheduler.ClientID
	if msg.ID != nil {
		origID = &scheduler.ClientID{Str: msg.ID.Str, Num: msg.ID.Num, IsStr: msg.ID.IsStr, IsNull: msg.ID.IsNull}
	}
	enqueuedAt := time.Now()
	costClass := scheduler.ClassifyCost(msg.Method) // §F8 dispatch-point classification
	req := &scheduler.Request{
		RequestID:   identity.RequestID(fmt.Sprintf("%s-%v", msg.Method, msg.ID)),
		OriginalID:  origID,
		Workspace:   s.workspaceID,
		Priority:    priority,
		CostClass:   costClass,
		Snapshot:    snap,
		CoalesceKey: coalesceKeyFor(msg, snap),
		EnqueuedAt:  enqueuedAt,
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			// INV-SNAPSHOT-002: the request serves from exactly this captured
			// snapshot. D11 staleness is NOT checked here — a queued request
			// behind a newer edit is still valid (C6 offers ContentModified
			// only where a mutating result must be fresh; see handleRename).
			// Gating state transitions here would drop accepted didChange
			// edits and violate PROT-SYNC-001.
			if phaseSpan != nil {
				phaseSpan.sample.SchedulerQueueWaitNS = elapsedNanoseconds(enqueuedAt)
				ctx = withCompletionPhaseSpan(ctx, phaseSpan)
			}
			dispatchStarted := time.Time{}
			if phaseSpan != nil {
				dispatchStarted = time.Now()
			}
			resp := s.dispatcher.Dispatch(withSnapshot(ctx, snap), msg)
			if phaseSpan != nil {
				phaseSpan.sample.DispatcherCallNS = elapsedNanoseconds(dispatchStarted)
				phaseSpan.dispatcherReturnedAt = time.Now()
			}
			return resp, nil
		},
	}
	// §F9: the server default deadline bounds the request's whole scheduled
	// lifetime (queue wait included). Expiry makes the scheduler stop the
	// work and finish the request with context.DeadlineExceeded, which
	// drainResult projects to the typed RequestCancelled response.
	if d := s.requestDeadline(costClass); d > 0 {
		req.Deadline = enqueuedAt.Add(d)
	}

	if reqID != "" {
		s.mu.Lock()
		s.inflight[reqID] = req
		if phaseSpan != nil {
			if s.completionPhaseSpans == nil {
				s.completionPhaseSpans = make(map[string]*completionPhaseTraceSpan)
			}
			s.completionPhaseSpans[reqID] = phaseSpan
		}
		s.mu.Unlock()
	}

	submitStarted := time.Time{}
	if phaseSpan != nil {
		phaseSpan.sample.SchedulePrepareNS = elapsedNanoseconds(phaseSpan.serverStartedAt)
		submitStarted = time.Now()
	}
	result := s.scheduler.Submit(req)
	if phaseSpan != nil {
		phaseSpan.sample.SchedulerSubmitNS = elapsedNanoseconds(submitStarted)
	}
	switch result {
	case scheduler.Admitted:
		// Read result in background goroutine to avoid blocking the main loop.
		go s.drainResult(req, reqID)
	case scheduler.RejectedQueueFull:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler queue full")
		s.finishQueuedCompletionTrace(reqID, "rejected_queue_full")
	case scheduler.RejectedCancelled:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler cancelled")
		s.finishQueuedCompletionTrace(reqID, "rejected_cancelled")
	default:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler rejected")
		s.finishQueuedCompletionTrace(reqID, "rejected")
	}
}

func (s *Server) beginScheduledPhaseTrace(msg *jsonrpc.Message) *completionPhaseTraceSpan {
	if s.completionPhaseTrace == nil || msg == nil || msg.ID == nil {
		return nil
	}
	uri := ""
	switch msg.Method {
	case "textDocument/completion":
		var params CompletionParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.TextDocument.URI == "" {
			return nil
		}
		uri = params.TextDocument.URI
	case "omnilsp/status":
		// Status is the soak's scheduler-liveness probe; its phase samples
		// split a slow status round trip into scheduler queue wait (scheduling),
		// dispatcher time (candidate handler), and response write (transport)
		// without adding a separate observation path.
	default:
		return nil
	}
	span := s.completionPhaseTrace.begin(msg.Method, "", uri, rawJSONRPCRequestID(msg.ID))
	if span != nil {
		span.serverStartedAt = time.Now()
		span.serverManaged = true
	}
	return span
}

func (s *Server) takeCompletionPhaseTrace(reqID string) *completionPhaseTraceSpan {
	if reqID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	span := s.completionPhaseSpans[reqID]
	delete(s.completionPhaseSpans, reqID)
	return span
}

func (s *Server) finishQueuedCompletionTrace(reqID, outcome string) {
	span := s.takeCompletionPhaseTrace(reqID)
	if span == nil {
		return
	}
	span.sample.ResponseOutcome = outcome
	span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
	span.finish()
}

func (s *Server) sendCompletionPhaseResponse(span *completionPhaseTraceSpan, msg *jsonrpc.Message) {
	if span == nil {
		if msg != nil {
			_ = s.send(msg)
		}
		return
	}
	if msg == nil {
		span.sample.ResponseOutcome = "missing_response"
		span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
		span.finish()
		return
	}
	writeStarted := time.Now()
	err := s.send(msg)
	span.sample.ResponseWriteNS = elapsedNanoseconds(writeStarted)
	span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
	switch {
	case err != nil:
		span.sample.ResponseOutcome = "write_failed"
	case msg.Error != nil:
		span.sample.ResponseOutcome = "error_response"
	default:
		span.sample.ResponseOutcome = "success"
	}
	span.finish()
}

// dispatchInline executes a state-transition or lifecycle message directly
// on the read-loop goroutine, preserving strict arrival order (F1). The C2
// lifecycle gate still applies; responses are sent synchronously.
func (s *Server) dispatchInline(msg *jsonrpc.Message) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if !s.lifecycleAllows(msg.Method) {
		s.respondLifecycleError(msg)
		return
	}
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp != nil {
		_ = s.send(resp)
	}
}

// coalescableMethods are read-only, idempotent queries whose result depends
// solely on (method, document position, snapshot revision) — safe to share
// one execution between concurrent identical requests (F11/J6). Mutating or
// side-effecting methods are deliberately excluded.
var coalescableMethods = map[string]bool{
	"textDocument/hover":      true,
	"textDocument/definition": true,
	"textDocument/references": true,
}

// coalesceParams is the minimal params projection needed for the key.
type coalesceParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"position"`
}

// coalesceKeyFor builds the F11 single-flight key for a coalescable query,
// or "" when the request must never join. Unparseable params yield "" (the
// request still executes; it just cannot share work).
func coalesceKeyFor(msg *jsonrpc.Message, snap *snapshot.Snapshot) string {
	if !coalescableMethods[msg.Method] || snap == nil || len(msg.Params) == 0 {
		return ""
	}
	var p coalesceParams
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.TextDocument.URI == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%d|%d",
		msg.Method, p.TextDocument.URI, snap.Revision(), p.Position.Line, p.Position.Character)
}

// lifecycleAllows enforces the C2 lifecycle state machine at admission.
// Before initialize completes only initialization-safe messages are accepted;
// after shutdown only exit; after exit nothing.
func (s *Server) lifecycleAllows(method string) bool {
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	switch st {
	case StateUninitialized, StateInitializing:
		switch method {
		case "initialize", "initialized", "exit":
			return true
		}
		return false
	case StateRunning:
		return true
	case StateShuttingDown:
		return method == "exit"
	default: // StateExited and unknown states accept nothing.
		return false
	}
}

// respondLifecycleError rejects a request that violates lifecycle ordering.
// Notifications are silently dropped per LSP semantics — but counted (§K).
func (s *Server) respondLifecycleError(msg *jsonrpc.Message) {
	s.metrics.RejectsTotal.Inc(1)
	if !msg.IsRequest() || msg.ID == nil {
		return
	}
	resp := jsonrpc.NewErrorResponse(*msg.ID, jsonrpc.InvalidRequest,
		fmt.Sprintf("method %q not allowed in state %s", msg.Method, s.State()), nil)
	_ = s.send(resp)
}

// drainResult reads the scheduled request result and sends the response.
// reqID is the JSON-RPC request ID key used for $/cancelRequest mapping (C7).
// drainInflight waits (bounded) for scheduled requests whose responses are
// still being written back by drainResult goroutines. Without this, a client
// that closes the stream right after its last request can race the response
// write: the session ends before "result" hits the wire (observed as a replay
// divergence under load). A timeout is an incomplete lifecycle, not success.
func (s *Server) drainInflight(maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		n := len(s.inflight)
		s.mu.RUnlock()
		if n == 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.RLock()
	n := len(s.inflight)
	s.mu.RUnlock()
	if n == 0 {
		return nil
	}
	return fmt.Errorf("shutdown: %d requests remain after %s", n, maxWait)
}

func (s *Server) drainResult(req *scheduler.Request, reqID string) {
	defer s.unregisterInflight(reqID)
	phaseSpan := s.takeCompletionPhaseTrace(reqID)
	phaseTraceFinished := false
	// F16: a panicking result path must not kill the process; the request
	// simply never gets a protocol response (logged below).
	defer func() {
		if r := recover(); r != nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: drainResult panic for %s: %v\n", reqID, r)
		}
		if phaseSpan != nil && !phaseTraceFinished {
			phaseSpan.sample.ResponseOutcome = "drain_result_panic"
			phaseSpan.sample.ServerRoundTripNS = elapsedNanoseconds(phaseSpan.serverStartedAt)
			phaseSpan.finish()
		}
	}()
	res := <-req.Result
	if phaseSpan != nil && !phaseSpan.dispatcherReturnedAt.IsZero() {
		phaseSpan.sample.SchedulerResultDeliveryNS = elapsedNanoseconds(phaseSpan.dispatcherReturnedAt)
	}
	if res.Err != nil {
		// Notifications have no wire ID to answer: a scheduler-level failure
		// (e.g. §F9 deadline expiry while queued) must stay silent instead of
		// emitting a response with a fabricated ID.
		if req.OriginalID == nil {
			security.FprintfRedacted(os.Stderr, "omnilsp: scheduled notification %s terminated: %v\n", req.RequestID, res.Err)
			return
		}
		// Echo the client's wire ID when we captured it; fall back to the
		// internal RequestID string only for requests that had none.
		respID := parseRequestID(req.RequestID)
		if req.OriginalID != nil {
			cid := *req.OriginalID
			respID = jsonrpc.RequestID{Str: cid.Str, Num: cid.Num, IsStr: cid.IsStr, IsNull: cid.IsNull}
		}
		if ierrors.IsKind(res.Err, ierrors.ErrCancelled) || ctxErr(res.Err) {
			// LSP cancellation is advisory: the original request still needs a
			// terminal response. RequestCancelled tells the client that work was
			// stopped without leaving its request ID pending forever.
			msg := jsonrpc.NewErrorResponse(respID, jsonrpc.RequestCancelled, "request cancelled", nil)
			s.sendCompletionPhaseResponse(phaseSpan, msg)
			phaseTraceFinished = phaseSpan != nil
			return
		}
		errText := projectBackendError(s, res.Err)
		msg := jsonrpc.NewErrorResponse(
			respID,
			jsonrpc.InternalError,
			errText,
			nil,
		)
		s.sendCompletionPhaseResponse(phaseSpan, msg)
		phaseTraceFinished = phaseSpan != nil
		return
	}
	// res.Value is *jsonrpc.Message from dispatcher.Dispatch
	if respMsg, ok := res.Value.(*jsonrpc.Message); ok && respMsg != nil {
		s.sendCompletionPhaseResponse(phaseSpan, respMsg)
	} else {
		s.sendCompletionPhaseResponse(phaseSpan, nil)
	}
	phaseTraceFinished = phaseSpan != nil
}

func ctxErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func (s *Server) unregisterInflight(reqID string) {
	if reqID == "" {
		return
	}
	s.mu.Lock()
	delete(s.inflight, reqID)
	s.mu.Unlock()
}

// cancelRequest implements the server side of $/cancelRequest (C7): it looks
// up the in-flight scheduled request and cancels it. Unknown IDs are ignored.
func (s *Server) cancelRequest(id jsonrpc.RequestID) {
	key := requestIDKey(id)
	s.mu.RLock()
	req := s.inflight[key]
	s.mu.RUnlock()
	if req != nil {
		req.Cancel()
	}
}

// requestIDKey builds a map key from a JSON-RPC request ID. The namespaces
// must be injective: id null (non-conformant JSON-RPC, but it decodes into a
// non-nil Message.ID and reaches the scheduler) used to fold into "n:0"
// together with the numeric id 0 because IsNull leaves Num at its zero value,
// letting two concurrent in-flight requests overwrite each other's inflight
// entry, misrouting $/cancelRequest and undercounting drainInflight
// (bug round 20 A#7, 圈 3).
func requestIDKey(id jsonrpc.RequestID) string {
	if id.IsNull {
		return "z:null"
	}
	if id.IsStr {
		return "s:" + id.Str
	}
	return fmt.Sprintf("n:%d", id.Num)
}

// snapshotCtxKey carries the request's captured snapshot through the handler
// chain (INV-SNAPSHOT-002: exactly one snapshot per request).
type snapshotCtxKey struct{}

type evidenceStageCtxKey struct{}

type evidenceStage struct {
	mu      sync.Mutex
	records []evidenceRecord
}

func withEvidenceStage(ctx context.Context, stage *evidenceStage) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, evidenceStageCtxKey{}, stage)
}

func (s *evidenceStage) append(record evidenceRecord) {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
}

func (s *evidenceStage) evidence() []identity.Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []identity.Evidence
	for _, record := range s.records {
		out = append(out, record.Ev...)
	}
	return out
}

// SetSemanticResponseObserver installs a recording hook. The hook must not
// mutate the provided evidence slice and must be safe for concurrent calls.
func (s *Server) SetSemanticResponseObserver(observer func(jsonrpc.RequestID, []identity.Evidence)) {
	s.mu.Lock()
	s.semanticResponseObserver = observer
	s.mu.Unlock()
}

func (s *Server) observeSemanticResponse(id jsonrpc.RequestID, evidence []identity.Evidence) {
	s.mu.RLock()
	observer := s.semanticResponseObserver
	s.mu.RUnlock()
	if observer != nil {
		observer(id, evidence)
	}
}

func (s *evidenceStage) commit(server *Server) {
	s.mu.Lock()
	records := append([]evidenceRecord(nil), s.records...)
	s.records = nil
	s.mu.Unlock()
	for _, record := range records {
		server.appendEvidence(record)
	}
}

func withSnapshot(ctx context.Context, snap *snapshot.Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, snap)
}

// snapshotFromCtx returns the captured snapshot, or nil when a handler runs
// outside the scheduler path (direct Dispatch in tests).
func snapshotFromCtx(ctx context.Context) *snapshot.Snapshot {
	if v, ok := ctx.Value(snapshotCtxKey{}).(*snapshot.Snapshot); ok {
		return v
	}
	return nil
}

// recordEvidence appends one §B4 evidence record to the explain ring.
// Callers: envelope-unwrapping semantic handlers.
func (s *Server) recordEvidence(ctx context.Context, method, uri string, status identity.ResultStatus, completeness identity.Completeness, ev []identity.Evidence, diag []string) {
	record := evidenceRecord{
		Method: method, URI: canonicalDocumentURI(uri), Ev: ev, Diag: diag, At: time.Now().UTC(),
		Status: status, Completeness: completeness,
	}
	if stage, ok := ctx.Value(evidenceStageCtxKey{}).(*evidenceStage); ok && stage != nil {
		stage.append(record)
		return
	}
	s.appendEvidence(record)
}

func (s *Server) appendEvidence(record evidenceRecord) {
	s.mu.Lock()
	s.evRing[s.evRingNext] = record
	s.evRingNext = (s.evRingNext + 1) % len(s.evRing)
	s.mu.Unlock()
}

// recentEvidence returns all live ring entries oldest-first.
func (s *Server) recentEvidence() []evidenceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]evidenceRecord, 0, len(s.evRing))
	for i := 0; i < len(s.evRing); i++ {
		idx := (s.evRingNext + i) % len(s.evRing)
		r := s.evRing[idx]
		if r.At.IsZero() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// classifyPriority maps LSP methods to scheduler priority classes (F3).
func (s *Server) classifyPriority(msg *jsonrpc.Message) scheduler.Priority {
	switch msg.Method {
	case "textDocument/completion", "textDocument/signatureHelp":
		return scheduler.PriorityCompletion
	case "textDocument/hover":
		return scheduler.PriorityHover
	case "textDocument/definition", "textDocument/declaration",
		"textDocument/typeDefinition", "textDocument/implementation":
		return scheduler.PriorityDefinition
	case "textDocument/references", "textDocument/rename":
		return scheduler.PriorityReferences
	case "textDocument/diagnostic", "textDocument/codeAction":
		return scheduler.PriorityDiagnostics
	case "textDocument/semanticTokens/full", "textDocument/semanticTokens/range",
		"textDocument/inlayHint":
		return scheduler.PrioritySemanticTokens
	case "$/progress":
		return scheduler.PriorityMaintenance
	default:
		return scheduler.PriorityMaintenance
	}
}

func (s *Server) send(msg *jsonrpc.Message) error {
	s.mu.RLock()
	t := s.transport
	s.mu.RUnlock()
	if t == nil {
		return fmt.Errorf("no transport")
	}
	return t.Write(context.Background(), msg)
}

func (s *Server) respondRejected(msg *jsonrpc.Message, reason string) {
	s.metrics.RejectsTotal.Inc(1)
	if !msg.IsRequest() || msg.ID == nil {
		return
	}
	resp := jsonrpc.NewErrorResponse(*msg.ID, jsonrpc.RequestFailed, reason, nil)
	_ = s.send(resp)
}

func parseRequestID(id identity.RequestID) jsonrpc.RequestID {
	return jsonrpc.RequestID{Str: string(id), IsStr: len(id) > 0}
}

// registerHandlers registers all LSP method handlers.
func (s *Server) registerHandlers() {
	s.dispatcher.Register("initialize", s.handleInitialize)
	s.dispatcher.Register("initialized", s.handleInitialized)
	s.dispatcher.Register("shutdown", s.handleShutdown)
	s.dispatcher.Register("exit", s.handleExit)
	s.dispatcher.Register("$/cancelRequest", s.handleCancelRequest)
	s.dispatcher.Register("textDocument/didOpen", s.handleDidOpen)
	s.dispatcher.Register("textDocument/didChange", s.handleDidChange)
	s.dispatcher.Register("textDocument/didSave", s.handleDidSave)
	s.dispatcher.Register("textDocument/didClose", s.handleDidClose)
	s.dispatcher.Register("textDocument/hover", s.handleHover)
	s.dispatcher.Register("textDocument/completion", s.handleCompletion)
	s.dispatcher.Register("textDocument/definition", s.handleDefinition)
	s.dispatcher.Register("textDocument/declaration", s.handleDeclaration)
	s.dispatcher.Register("textDocument/documentSymbol", s.handleDocumentSymbol)
	s.dispatcher.Register("textDocument/references", s.handleReferences)
	s.dispatcher.Register("textDocument/rename", s.handleRename)
	s.dispatcher.Register("textDocument/semanticTokens/full", s.handleSemanticTokens)
	s.dispatcher.Register("textDocument/prepareRename", s.handlePrepareRename)
	s.dispatcher.Register("textDocument/diagnostic", s.handlePullDiagnostics)
	s.dispatcher.Register("workspace/didChangeWatchedFiles", s.handleDidChangeWatchedFiles)
	s.dispatcher.Register("workspace/didCreateFiles", s.handleDidCreateFiles)
	s.dispatcher.Register("workspace/didRenameFiles", s.handleDidRenameFiles)
	s.dispatcher.Register("workspace/didDeleteFiles", s.handleDidDeleteFiles)
	s.dispatcher.Register("textDocument/codeAction", s.handleCodeAction)
	s.dispatcher.Register("textDocument/signatureHelp", s.handleSignatureHelp)
	s.dispatcher.Register("textDocument/formatting", s.handleFormatting)
	s.dispatcher.Register("textDocument/inlayHint", s.handleInlayHints)
	s.dispatcher.Register("workspace/symbol", s.handleWorkspaceSymbol)
	// Custom extension namespace per goal.md §C12 (omnilsp/* only).
	s.dispatcher.Register("omnilsp/status", s.handleOmnilspStatus)
	s.dispatcher.Register("omnilsp/explain", s.handleOmnilspExplain)
	s.dispatcher.Register("omnilsp/backendStatus", s.handleOmnilspBackendStatus)
	s.dispatcher.Register("omnilsp/resultMeta", s.handleOmnilspResultMeta)
	s.dispatcher.Register("omnilsp/queryTrace", s.handleOmnilspQueryTrace)
	s.dispatcher.Register("omnilsp/indexStats", s.handleIndexStats)
	s.dispatcher.Register("omnilsp/reindex", s.handleReindex)
}

// State returns the current server state.
func (s *Server) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// facadeMutatingMethods are the VFS-mutating notifications registered in the
// dispatcher. The CallMethod facade admits them only in the same state the C2
// lifecycle gate would admit them over LSP — otherwise an MCP/HTTP adapter
// could mutate the VFS from any goroutine, bypassing lifecycle ordering.
var facadeMutatingMethods = map[string]bool{
	"textDocument/didOpen":   true,
	"textDocument/didChange": true,
	"textDocument/didSave":   true,
	"textDocument/didClose":  true,
}

// CallMethod is the canonical read-only facade for non-LSP protocol adapters
// (MCP tools, HTTP API). It routes through the same dispatcher — lifecycle
// gate, snapshot capture, backend resolution, evidence recording — so a
// query over MCP/HTTP behaves exactly like the same query over LSP
// (§C0: semantic behavior MUST NOT differ by transport; §C14: no snapshot
// bypass). Mutating methods are gated to the Ready (Running) state with C2-gate
// semantics; anything unregistered returns a typed method-not-found error.
func (s *Server) CallMethod(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if facadeMutatingMethods[method] {
		if s.State() != StateRunning {
			return nil, fmt.Errorf("method %q not allowed in state %s", method, s.State())
		}
		// Bug round 20 A#9（圈 3）：VFS 变异型通知必须与读循环共用同一把单写
		// 者锁（dispatchInline 的 mutationMu）——门面此前在调用方 goroutine
		// 直派 handleDidChange，version 门 + Update 的 check-then-act 可被同
		// 版本双编辑同时穿过，静默丢失其一（PROT-SYNC-001 形态）。锁纪律与
		// dispatchInline 完全一致：mutationMu 下调 Dispatch 本就是生产不变量
		// （读循环路径），不存在重入死锁面。
		s.mutationMu.Lock()
		defer s.mutationMu.Unlock()
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "facade", IsStr: true}, method, rawParams)
	resp := s.dispatcher.Dispatch(ctx, msg)
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

// HoverEnvelope returns the raw canonical hover envelope (§B5) for adapters
// that project evidence themselves (C14). Same routing as textDocument/hover.
func (s *Server) HoverEnvelope(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	uri = canonicalDocumentURI(uri)
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[*languages.HoverResult], error) {
		return be.Hover(ctx, languages.HoverRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc, Line: line, Column: col,
		})
	})
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	s.recordEvidence(ctx, "textDocument/hover", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

// DefinitionEnvelope is HoverEnvelope's counterpart for definitions.
func (s *Server) DefinitionEnvelope(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[[]languages.Location], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	uri = canonicalDocumentURI(uri)
	ctx, _, revision := s.captureEnvelopeInputs(ctx, uri, nil, 0)
	if result, ok := s.indexedLocationEnvelope(ctx, uri, line, col, false, persistentDefinition, revision); ok {
		var err error
		result, err = verifyEnvelopeFresh(s, ctx, result)
		if err != nil {
			return unavailableEnvelope[[]languages.Location](), err
		}
		s.recordEvidence(ctx, "textDocument/definition", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[[]languages.Location], error) {
		return be.Definition(ctx, languages.DefinitionRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc, Line: line, Column: col,
		})
	})
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	s.recordEvidence(ctx, "textDocument/definition", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

// ReferencesEnvelope is HoverEnvelope's counterpart for references.
func (s *Server) ReferencesEnvelope(ctx context.Context, uri string, line, col uint32, includeDecl bool) (identity.SemanticResult[[]languages.Location], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	uri = canonicalDocumentURI(uri)
	ctx, _, revision := s.captureEnvelopeInputs(ctx, uri, nil, 0)
	if result, ok := s.indexedLocationEnvelope(ctx, uri, line, col, includeDecl, persistentReferences, revision); ok {
		var err error
		result, err = verifyEnvelopeFresh(s, ctx, result)
		if err != nil {
			return unavailableEnvelope[[]languages.Location](), err
		}
		s.recordEvidence(ctx, "textDocument/references", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[[]languages.Location], error) {
		return be.References(ctx, languages.ReferencesRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc,
			Line: line, Column: col, IncludeDecl: includeDecl,
		})
	})
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	s.recordEvidence(ctx, "textDocument/references", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

func unavailableEnvelope[T any]() identity.SemanticResult[T] {
	return identity.SemanticResult[T]{
		Status:       identity.ResultUnavailable,
		Completeness: identity.CompletenessUnknown,
	}
}

func verifyEnvelopeFresh[T any](s *Server, ctx context.Context, result identity.SemanticResult[T]) (identity.SemanticResult[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[T](), err
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil || s.snapMgr == nil {
		return result, nil
	}
	current := s.snapMgr.Current()
	if current != nil && current.ID().Revision != captured.ID().Revision {
		return unavailableEnvelope[T](), ierrors.New(ierrors.ErrContentModified, "server.envelope",
			fmt.Sprintf("workspace changed since request (snapshot %d, current %d)", captured.ID().Revision, current.ID().Revision))
	}
	return result, nil
}

func (s *Server) indexedLocationEnvelope(ctx context.Context, uri string, line, col uint32, includeDecl bool, query persistentSemanticQuery, revision uint64) (identity.SemanticResult[[]languages.Location], bool) {
	if ctx.Err() != nil {
		return identity.SemanticResult[[]languages.Location]{}, false
	}
	if result, ok := s.goSnapshotSemanticLocations(ctx, uri, line, col, 1, revision, query, includeDecl); ok {
		return result, true
	}
	return s.persistentSemanticLocations(ctx, uri, line, col, 1, revision, query, includeDecl)
}

func (s *Server) captureEnvelopeInputs(ctx context.Context, uri string, source []byte, revision uint64) (context.Context, []byte, uint64) {
	if ctx == nil {
		ctx = context.Background()
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil && s.snapMgr != nil {
		captured = s.snapMgr.Current()
		if captured != nil {
			ctx = withSnapshot(ctx, captured)
		}
	}
	if captured != nil {
		revision = captured.ID().Revision
		if document := captured.Document(uri); document != nil {
			source = document.Content
		}
	}
	return ctx, source, revision
}

// semanticSource mirrors dispatchSemanticRequest's content resolution:
// captured snapshot document wins, then VFS overlay, then empty.
//
// §D12 hook: a registered virtual URI reads through to its host content so
// semantic calls never see an empty source for a virtual document. The full
// pipeline now exists as callable machinery on this seam — inbound region
// extraction via RegisterVirtualDocument (Host Document → Region Extraction
// → Virtual Document, backed by virtual.Extract/RegisterExtracted) and
// outbound coordinate projection via VirtualProjector (Virtual Result →
// Reverse Source Map → Host Result, backed by virtual.Projector).
// TODO(D12): threading the projector through every Envelope call site and
// the languages request/response structs stays deferred until a backend
// actually emits virtual documents — until then a virtual URI keeps
// receiving its host content, so request and result positions remain in one
// coordinate system.
func (s *Server) semanticSource(uri string) []byte {
	if s.virtualReg != nil {
		if host, err := s.virtualReg.ResolveHost(uri); err == nil {
			uri = host
		}
	}
	if snap := s.snapMgr.Current(); snap != nil {
		if doc := snap.Document(uri); doc != nil && doc.Content != nil {
			return doc.Content
		}
	}
	if src := s.vfs.Content(uri); src != nil {
		return src
	}
	return []byte{}
}

// VirtualRegistry exposes the §D12 virtual-document/source-map registry so
// adapters can register host↔virtual mappings and S3 gates can consult them.
func (s *Server) VirtualRegistry() *virtual.Registry { return s.virtualReg }

// RegisterVirtualDocument runs the §D12 inbound pipeline on entering host
// content (Host Document → Region Extraction → Virtual Document): the pieces
// are extracted from hostContent into the virtual slice and the derived
// source map is registered under virtualURI (replacing any previous mapping,
// per the registry contract). The returned slice is the virtual content a
// backend would eventually receive; the registry stores mappings, not text,
// so the caller keeps it alongside the host document and hands both back to
// VirtualProjector on the way out.
func (s *Server) RegisterVirtualDocument(virtualURI, hostURI string, hostContent []byte, pieces []virtual.Piece) ([]byte, error) {
	return s.virtualReg.RegisterExtracted(virtualURI, hostURI, hostContent, pieces)
}

// VirtualProjector builds the §D12 outbound projector (Virtual Result →
// Reverse Source Map → Host Result) for a registered virtual document over
// the caller's current host/virtual content, in the §C4-negotiated position
// encoding. Construction fails closed for unknown virtual URIs and for
// content that disagrees with the registered Exact segments.
func (s *Server) VirtualProjector(virtualURI string, hostContent, virtualContent []byte) (*virtual.Projector, error) {
	return virtual.NewProjector(s.virtualReg, virtualURI, hostContent, virtualContent,
		position.Encoding(s.negotiatedEncodingInt()))
}

// queryContext pairs the current snapshot revision with the backend's build
// context — the two identities every canonical request must carry.
func (s *Server) queryContext(be languages.Backend, uri string) (uint64, identity.BuildContextID) {
	snapRev := uint64(0)
	if snap := s.snapMgr.Current(); snap != nil {
		snapRev = snap.ID().Revision
	}
	return snapRev, backendBuildContext(be)
}

// SnapshotRevision returns the current snapshot revision for observability
// consumers such as the §P9 session recorder. Zero before first publish.
func (s *Server) SnapshotRevision() uint64 {
	if snap := s.snapMgr.Current(); snap != nil {
		return snap.Revision()
	}
	return 0
}

// projectBackendError enriches a failed request's error text with the
// backend's supervisor lifecycle message (§Q2): a crash becomes "Recovery in
// progress (crash #2, epoch 3)" instead of a bare "process terminated".
func projectBackendError(s *Server, err error) string {
	text := err.Error()
	var be *ierrors.Error
	if !errors.As(err, &be) || be.Op == "" {
		return text
	}
	s.mu.RLock()
	backend := s.languages[be.Op]
	s.mu.RUnlock()
	rep, ok := backend.(languages.StatusReporter)
	if !ok {
		return text
	}
	if status := rep.BackendStatusMessage(); status != "" {
		return text + " [" + status + "]"
	}
	return text
}
