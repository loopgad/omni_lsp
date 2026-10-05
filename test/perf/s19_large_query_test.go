package perf

// §S19 大查询 SLO 的三个非计时维度（goal.md §S19：cancelable 已由
// TestS19_ReferencesScalingCurve 覆盖；本文件补齐其余三维）：
//
//	progress-observable —— 带 workDoneToken 的大查询必须上报 $/progress
//	                     begin/end，且无 token 时保持沉默（§C8 投影绑定
//	                     大查询路径）；
//	bounded memory      —— 真实 Go 后端反复执行 3200 引用的大查询后，
//	                     堆增量与 goroutine 增量必须落在 soak 门同款界内；
//	no starvation       —— 大查询阻塞在后端内部时，P0/P1 交互查询必须
//	                     仍被调度服务（镜像 e2e no-starvation 证据形态）。
//
// 进度与隔离测试走 transport 层（复用 golden 的内存管道模式）：$/progress
// 与调度优先级都是传输层/调度器行为，CallMethod 门面（perf_test.go 计时用）
// 既不经调度器（server.go scheduleMessage 仅由 Run 读循环驱动），也无通知
// 出口，无法观测这两维。三者均不做 µs 级计时，5ms 轮询粒度无影响。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/server"
)

const s19LargeRefsCount = 800

// --- 大结果集 fake 后端 ----------------------------------------------------------

// s19LargeRefsBackend 把 fake 后端的 references 换成确定性的 800 处引用，
// 使 $/progress 断言真正绑定在「大查询」而非普通查询上。
type s19LargeRefsBackend struct{ fakeBackend }

func (b s19LargeRefsBackend) References(_ context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	locs := make([]languages.Location, 0, s19LargeRefsCount+1)
	for i := 0; i < s19LargeRefsCount; i++ {
		locs = append(locs, languages.Location{
			URI:   docURI,
			Range: languages.Range{StartLine: 4, StartCharacter: 8, EndLine: 4, EndCharacter: 9},
		})
	}
	if req.IncludeDecl {
		locs = append(locs, languages.Location{
			URI:   docURI,
			Range: languages.Range{StartLine: 3, StartCharacter: 5, EndLine: 3, EndCharacter: 11},
		})
	}
	return identity.SemanticResult[[]languages.Location]{Value: locs, Evidence: perfEvidence, Completeness: identity.Complete}, nil
}

// s19GatedRefsBackend 在进入 References 后阻塞，直到测试放行 —— 用于在
// 大查询在途期间观察 P0/P1 是否仍被服务。
type s19GatedRefsBackend struct {
	fakeBackend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *s19GatedRefsBackend) References(ctx context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return identity.SemanticResult[[]languages.Location]{}, ctx.Err()
	}
	return s19LargeRefsBackend{}.References(ctx, req)
}

// --- 内存管道 transport（模式取自 test/golden，本包不可跨目录复用）----------------

type s19PipeTransport struct {
	mu     sync.Mutex
	writes []*jsonrpc.Message
	in     chan *jsonrpc.Message
	done   chan struct{}
	once   sync.Once
}

func newS19PipeTransport() *s19PipeTransport {
	return &s19PipeTransport{in: make(chan *jsonrpc.Message, 32), done: make(chan struct{})}
}

func (p *s19PipeTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case m := <-p.in:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("transport closed")
	}
}

func (p *s19PipeTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	p.mu.Lock()
	p.writes = append(p.writes, msg)
	p.mu.Unlock()
	return nil
}

func (p *s19PipeTransport) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *s19PipeTransport) Done() <-chan struct{} { return p.done }

// send 投递一条原始 JSON-RPC 消息（客户端视角，同 golden 场景格式）。
func (p *s19PipeTransport) send(t *testing.T, raw string) {
	t.Helper()
	var msg jsonrpc.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("bad message %s: %v", raw, err)
	}
	select {
	case p.in <- &msg:
	case <-time.After(5 * time.Second):
		t.Fatalf("server read loop stalled sending %q", msg.Method)
	}
}

// waitResponse 轮询写出帧，返回 method 为空且 ID 匹配的响应（不匹配超时）。
func (p *s19PipeTransport) waitResponse(id int, timeout time.Duration) *jsonrpc.Message {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range p.snapshot() {
			if m.Method == "" && m.ID != nil && m.ID.Num == int64(id) {
				return m
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func (p *s19PipeTransport) snapshot() []*jsonrpc.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*jsonrpc.Message(nil), p.writes...)
}

// progressEvents 返回全部 $/progress 通知帧。
func (p *s19PipeTransport) progressEvents() []*jsonrpc.Message {
	var out []*jsonrpc.Message
	for _, m := range p.snapshot() {
		if m.Method == "$/progress" {
			out = append(out, m)
		}
	}
	return out
}

// --- 会话（新 Server + Run 读循环，同 golden 执行器）------------------------------

type s19Session struct {
	pt     *s19PipeTransport
	cancel context.CancelFunc
	done   chan error
}

// startS19Server 完成 C2 握手（initialize → initialized → didOpen 语料）。
// inline 方法在读循环内同步执行，后续查询按到达顺序必然看到已发布的语料。
func startS19Server(t *testing.T, be languages.Backend) *s19Session {
	t.Helper()
	srv := server.New(server.DefaultConfig())
	srv.RegisterBackend("go", be)
	pt := newS19PipeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	s := &s19Session{pt: pt, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- srv.Run(ctx, pt) }()

	pt.send(t, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"processId":1,"rootUri":"file:///w"}}`)
	if resp := pt.waitResponse(0, 5*time.Second); resp == nil || resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp)
	}
	pt.send(t, `{"jsonrpc":"2.0","method":"initialized","params":{}}`)
	pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":%s,"languageId":"go","version":1,"text":%s}}}`,
		s19JSONString(docURI), s19JSONString(corpus)))
	return s
}

func (s *s19Session) stop() {
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			_ = err // 读循环因 transport 关闭/ctx 取消退出属预期；Run 自身已做 drain
		}
	case <-time.After(5 * time.Second):
	}
}

func s19JSONString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// --- 维度一：progress-observable -------------------------------------------------

type s19ProgressValue struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type s19ProgressEvent struct {
	Token string           `json:"token"`
	Value s19ProgressValue `json:"value"`
}

// TestS19_LargeQueryProgressObservable pins §S19 progress-observability on the
// large-query path: a token-bearing 800-result references query must emit
// begin (title "Finding references") then end for that exact token before the
// terminal response, and the same query without a token must stay silent.
func TestS19_LargeQueryProgressObservable(t *testing.T) {
	s := startS19Server(t, s19LargeRefsBackend{})
	defer s.stop()

	s.pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":10,"method":"textDocument/references","params":{"textDocument":{"uri":%s},"position":{"line":%d,"character":%d},"context":{"includeDeclaration":true},"workDoneToken":%s}}`,
		s19JSONString(docURI), hoverLine, hoverChar, s19JSONString("s19-progress")))
	resp := s.pt.waitResponse(10, 10*time.Second)
	if resp == nil {
		t.Fatal("large references query did not respond")
	}
	if resp.Error != nil {
		t.Fatalf("large references query failed: %s", resp.Error.Message)
	}
	var locations []struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(resp.Result, &locations); err != nil {
		t.Fatalf("decode references result: %v", err)
	}
	if len(locations) != s19LargeRefsCount+1 {
		t.Fatalf("large query returned %d locations, want %d (assertion must bind to the large path)", len(locations), s19LargeRefsCount+1)
	}

	events := s.pt.progressEvents()
	if len(events) < 2 {
		t.Fatalf("expected >=2 $/progress notifications for token-bearing large query, got %d", len(events))
	}
	var first, last s19ProgressEvent
	if err := json.Unmarshal(events[0].Params, &first); err != nil {
		t.Fatalf("decode first progress event: %v", err)
	}
	if err := json.Unmarshal(events[len(events)-1].Params, &last); err != nil {
		t.Fatalf("decode last progress event: %v", err)
	}
	if first.Token != "s19-progress" || first.Value.Kind != "begin" || first.Value.Title != "Finding references" {
		t.Errorf("begin event malformed: token=%q kind=%q title=%q", first.Token, first.Value.Kind, first.Value.Title)
	}
	if last.Token != "s19-progress" || last.Value.Kind != "end" {
		t.Errorf("end event malformed: token=%q kind=%q", last.Token, last.Value.Kind)
	}

	// 无 token 的同型查询必须保持沉默（§C8：server 从不发明 token）。
	before := len(events)
	s.pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":11,"method":"textDocument/references","params":{"textDocument":{"uri":%s},"position":{"line":%d,"character":%d},"context":{"includeDeclaration":true}}}`,
		s19JSONString(docURI), hoverLine, hoverChar))
	if resp := s.pt.waitResponse(11, 10*time.Second); resp == nil || resp.Error != nil {
		t.Fatalf("tokenless references query failed: %+v", resp)
	}
	if after := len(s.pt.progressEvents()); after != before {
		t.Errorf("tokenless large query emitted %d extra $/progress events", after-before)
	}
}

// --- 维度二：no starvation of P0/P1 ----------------------------------------------

// TestS19_LargeQueryDoesNotStarveInteractiveQueries pins the §S19 isolation
// dimension: while one large references query is blocked inside the backend,
// hover (P0) and completion (P0) must still be admitted and served, and the
// large query must remain in flight until the backend releases it.
func TestS19_LargeQueryDoesNotStarveInteractiveQueries(t *testing.T) {
	be := &s19GatedRefsBackend{entered: make(chan struct{}), release: make(chan struct{})}
	s := startS19Server(t, be)
	defer s.stop()

	s.pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":20,"method":"textDocument/references","params":{"textDocument":{"uri":%s},"position":{"line":%d,"character":%d},"context":{"includeDeclaration":true},"workDoneToken":%s}}`,
		s19JSONString(docURI), hoverLine, hoverChar, s19JSONString("s19-starvation")))
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("large references query never reached the backend")
	}

	start := time.Now()
	s.pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":21,"method":"textDocument/hover","params":{"textDocument":{"uri":%s},"position":{"line":%d,"character":%d}}}`,
		s19JSONString(docURI), hoverLine, hoverChar))
	s.pt.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":22,"method":"textDocument/completion","params":{"textDocument":{"uri":%s},"position":{"line":%d,"character":%d}}}`,
		s19JSONString(docURI), hoverLine, hoverChar))
	hresp := s.pt.waitResponse(21, 5*time.Second)
	if hresp == nil || hresp.Error != nil {
		t.Fatalf("hover (P0) not served while large query in flight: %+v", hresp)
	}
	cresp := s.pt.waitResponse(22, 5*time.Second)
	if cresp == nil || cresp.Error != nil {
		t.Fatalf("completion (P0) not served while large query in flight: %+v", cresp)
	}
	t.Logf("P0 hover+completion served in %s while the large query was blocked in the backend", time.Since(start))

	if inFlight := s.pt.waitResponse(20, 50*time.Millisecond); inFlight != nil {
		t.Fatal("large references query returned while the backend gate was still closed")
	}

	close(be.release)
	rresp := s.pt.waitResponse(20, 10*time.Second)
	if rresp == nil || rresp.Error != nil {
		t.Fatalf("large references query did not complete after release: %+v", rresp)
	}
	var locations []struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(rresp.Result, &locations); err != nil {
		t.Fatalf("decode released references result: %v", err)
	}
	if len(locations) != s19LargeRefsCount+1 {
		t.Fatalf("released large query returned %d locations, want %d", len(locations), s19LargeRefsCount+1)
	}
}

// --- 维度三：bounded memory ------------------------------------------------------

// TestS19_RepeatedLargeQueriesBoundedMemory pins the §S19 bounded-memory
// dimension on the real Go backend: 25 repeated 3200-result reference walks
// after warm-up must not grow the heap or the goroutine count. The bounds
// mirror the soak gate's documented post-run bounds (256 MiB additional heap,
// at most 50 additional goroutines — docs/soak-nightly.md).
func TestS19_RepeatedLargeQueriesBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	const size = 3200
	be, req := newGoReferencesFixture(t, size)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // warm the package loader and caches
		res, err := be.References(ctx, req)
		if err != nil || len(res.Value) < size {
			t.Fatalf("warm references@%d: %d locations, err=%v", size, len(res.Value), err)
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutinesBefore := runtime.NumGoroutine()

	const iterations = 25
	for i := 0; i < iterations; i++ {
		res, err := be.References(ctx, req)
		if err != nil || len(res.Value) < size {
			t.Fatalf("references iteration %d: %d locations, err=%v", i, len(res.Value), err)
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	goroutineGrowth := runtime.NumGoroutine() - goroutinesBefore
	t.Logf("25 repeated 3200-result reference walks: heap delta %.1f MiB, goroutine delta %+d",
		float64(heapGrowth)/(1<<20), goroutineGrowth)
	if heapGrowth > 256<<20 {
		t.Errorf("heap grew %.1f MiB over %d repeated large queries; §S19 requires bounded memory",
			float64(heapGrowth)/(1<<20), iterations)
	}
	if goroutineGrowth > 50 {
		t.Errorf("goroutine count grew by %d over %d repeated large queries; per-query goroutines must not accumulate", goroutineGrowth, iterations)
	}
}
