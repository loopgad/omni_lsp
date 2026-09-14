package nested

// Supervision (§G2/F15), build-context identity (§E0), and request-wait
// tests for the shared nested-LSP bridge. All run against injected fake
// processes — no real language server required.

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/runtime/supervisor"
)

func fastSup() *supervisor.Config {
	return &supervisor.Config{
		MaxConsecutiveCrashes: 3,
		InitialBackoff:        time.Millisecond,
		MaxBackoff:            5 * time.Millisecond,
		HungGrace:             time.Second,
	}
}

func fakeStart(c *Conn, spawns *atomic.Int32) error {
	spawns.Add(1)
	r, _ := io.Pipe()
	c.Attach(nil, nopWriteCloser{}, r) // nil cmd + sink stdin: same EOF lifecycle as a real spawn
	c.MarkReady()
	return nil
}

// nopWriteCloser absorbs writes so request paths never block on a pipe with
// no reader — only the stdout side drives lifecycle in these tests.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func newTestConn(t *testing.T, spawns *atomic.Int32) *Conn {
	t.Helper()
	c := New(Config{
		Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
		Sup: fastSup(),
	})
	c.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := c.StartSupervised(); err != nil {
		t.Fatalf("initial spawn: %v", err)
	}
	return c
}

func TestG2_CrashFailsPendingFastAndRestarts(t *testing.T) {
	var spawns atomic.Int32
	c := newTestConn(t, &spawns)

	ch := c.RegisterPending(42)

	// Kill: close the stdout pipe → readLoop sees EOF → supervision kicks in.
	if err := c.stdout.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-ch:
		if msg.Error == nil {
			t.Fatal("pending request must fail fast with an error response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request never failed after crash")
	}

	// Supervisor restarts after backoff (fake spawn increments the counter).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if spawns.Load() >= 2 && c.sup.State() == supervisor.StateReady {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := spawns.Load(); got < 2 {
		t.Fatalf("restarts = %d, want >= 2 (initial + one recovery)", got)
	}
	if st := c.sup.State(); st != supervisor.StateReady {
		t.Fatalf("state = %v, want ready after successful restart", st)
	}
}

func TestG2_AttachFailsOrphanedPending(t *testing.T) {
	var spawns atomic.Int32
	c := newTestConn(t, &spawns)

	ch := c.RegisterPending(7)

	// 模拟崩溃窗口期后重连：Attach 替换 pending 表时，
	// 旧表中残留的请求必须被快速失败，而不是等到超时。
	r, _ := io.Pipe()
	c.Attach(nil, nopWriteCloser{}, r)

	select {
	case msg := <-ch:
		if msg.Error == nil {
			t.Fatal("Attach 后残留 pending 必须收到错误响应")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach 替换 pending 表时丢弃了旧请求，未快速失败")
	}
	c.Close()
}

func TestG2_RepeatedCrashesQuarantine(t *testing.T) {
	var spawns atomic.Int32
	c := newTestConn(t, &spawns)

	// Three consecutive crashes must reach the quarantine threshold (max=3).
	for i := 0; i < 3; i++ {
		c.HandleProcessExit(errors.New("synthetic crash"))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.sup.State() == supervisor.StateQuarantined {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.sup.State() != supervisor.StateQuarantined {
		t.Fatalf("state = %v, want quarantined after repeated crashes", c.sup.State())
	}

	before := spawns.Load()
	time.Sleep(30 * time.Millisecond)
	if spawns.Load() != before {
		t.Error("quarantined backend must not respawn")
	}
}

func TestSendRequest_TimeoutAndCancel(t *testing.T) {
	var spawns atomic.Int32
	c := newTestConn(t, &spawns)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SendRequest(ctx, "any", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx err = %v, want context.Canceled", err)
	}

	c.cfg.RequestTimeout = 10 * time.Millisecond
	start := time.Now()
	if _, err := c.SendRequest(context.Background(), "slow", nil); err == nil {
		t.Error("expected timeout error with no responder")
	} else if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v, want ~10ms", d)
	}
}

func TestClose_IdleConnectionIsSafe(t *testing.T) {
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})

	if err := c.Close(); err != nil {
		t.Fatalf("Close on idle connection: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("repeated Close on idle connection: %v", err)
	}
}

func TestAttach_OldReaderCannotFailNewPending(t *testing.T) {
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	oldReader, oldWriter := io.Pipe()
	newReader, _ := io.Pipe()
	c.Attach(nil, nopWriteCloser{}, oldReader)
	c.Attach(nil, nopWriteCloser{}, newReader)
	defer c.Close()

	pending := c.RegisterPending(99)
	if err := oldWriter.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-pending:
		t.Fatalf("old reader failed new pending request: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

type trackingReadCloser struct {
	closed atomic.Bool
	data   atomic.Value
}

func (c *trackingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (c *trackingReadCloser) Write(p []byte) (int, error) {
	c.data.Store(string(p))
	return len(p), nil
}
func (c *trackingReadCloser) Close() error {
	c.closed.Store(true)
	return nil
}

func TestAttach_AfterCloseReleasesIncomingResources(t *testing.T) {
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	c.Close()

	stdin := &trackingReadCloser{}
	stdout := &trackingReadCloser{}
	c.Attach(nil, stdin, stdout)

	if !stdin.closed.Load() || !stdout.closed.Load() {
		t.Fatal("late Attach did not release incoming resources")
	}
}

func TestClose_SendsShutdownNotification(t *testing.T) {
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	stdin := &trackingReadCloser{}
	stdout := &trackingReadCloser{}
	c.Attach(nil, stdin, stdout)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, ok := stdin.data.Load().(string)
	if !ok || !strings.Contains(raw, `"method":"shutdown"`) {
		t.Fatalf("shutdown notification missing from %q", raw)
	}
}

// errWriteCloser fails every write, simulating a dead stdin pipe so the
// initialize handshake cannot succeed.
type errWriteCloser struct{}

func (errWriteCloser) Write(p []byte) (int, error) { return 0, errors.New("pipe broken") }
func (errWriteCloser) Close() error                { return nil }

func TestInitialize_PropagatesHandshakeError(t *testing.T) {
	var spawns atomic.Int32
	c := New(Config{
		Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
		Sup: fastSup(),
	})
	c.cfg.Start = func(c *Conn) error {
		spawns.Add(1)
		r, _ := io.Pipe()
		c.Attach(nil, errWriteCloser{}, r)
		c.MarkReady()
		return nil
	}
	if err := c.StartSupervised(); err != nil {
		t.Fatalf("initial spawn: %v", err)
	}
	defer c.Close()

	// 握手写入失败必须上传播，而不是静默吞掉后照常 MarkReady。
	if err := c.Initialize(); err == nil {
		t.Fatal("Initialize 必须返回握手错误，得到 nil")
	}
}

func TestE_NestedBuildContextID(t *testing.T) {
	v := func() (string, error) { return "clangd version 18.1.3\n", nil }
	cfg := Config{Name: "clangd", Lang: "cpp", WorkDir: "/w"}

	id1 := New(withProbe(cfg, v)).BuildContextID()
	id2 := New(withProbe(cfg, v)).BuildContextID() // fresh conn, same inputs
	if id1 != id2 || id1 == "" {
		t.Fatalf("unstable identity: %q vs %q", id1, id2)
	}
	if string(id1)[:11] != "cpp:sha256:" {
		t.Errorf("id %q lacks cpp:sha256: prefix", id1)
	}

	bad := New(withProbe(cfg, func() (string, error) { return "", errors.New("no clangd") }))
	if got := bad.BuildContextID(); got != "cpp:sha256:unavailable" {
		t.Errorf("probe failure ID = %q, want unavailable marker", got)
	}
}

func withProbe(cfg Config, probe func() (string, error)) Config {
	cfg.VersionProbe = probe
	cfg.ParseVersion = parseVersionField
	return cfg
}

// parseVersionField extracts the token after "version" — the shared shape of
// clangd/rust-analyzer/tsc --version output. Language bridges inject this or
// their own parser.
func parseVersionField(output string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("unparseable version output")
}

// TestG5_WatchdogRestartsSilentWorkerWithInflight pins the hung-worker path:
// pending requests + silence beyond HungGrace must fail the requests and
// recycle the process, while an idle silent process is left alone.
func TestG5_WatchdogRestartsSilentWorkerWithInflight(t *testing.T) {
	sup := fastSup()
	sup.HungGrace = 60 * time.Millisecond // watchdog ticks every 30ms
	spawns := &atomic.Int32{}
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: sup, RequestTimeout: 120 * time.Millisecond})
	c.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := c.StartSupervised(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	firstSpawn := spawns.Load()

	// Park one in-flight request: RegisterPending without a responder.
	ch := c.RegisterPending(7777)

	// The watchdog must fail the parked request within a few grace windows.
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case resp := <-ch:
			if resp == nil || resp.Error == nil {
				t.Fatalf("expected error response for hung request, got %+v", resp)
			}
			goto parked
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("watchdog never failed the in-flight request")
		}
		time.Sleep(10 * time.Millisecond)
	}
parked:
	// ...and the supervisor must recycle the process afterwards.
	recycleDeadline := time.Now().Add(3 * time.Second)
	for spawns.Load() <= firstSpawn {
		if time.Now().After(recycleDeadline) {
			t.Fatalf("watchdog did not recycle the worker (spawns=%d)", spawns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestG5_WatchdogSparesIdleWorker: no pending requests ⇒ silence is fine.
func TestG5_WatchdogSparesIdleWorker(t *testing.T) {
	sup := fastSup()
	sup.HungGrace = 60 * time.Millisecond
	spawns := &atomic.Int32{}
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: sup, RequestTimeout: 120 * time.Millisecond})
	c.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := c.StartSupervised(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(400 * time.Millisecond)
	if spawns.Load() != 1 {
		t.Fatalf("idle worker was recycled %d times", spawns.Load()-1)
	}
}

// TestG5_WatchdogSparesLegitimateLongQuery pins the alignment fix: a request
// still inside its RequestTimeout window (activity refreshed mid-flight) must
// not be recycled even though silence exceeds HungGrace.
func TestG5_WatchdogSparesLegitimateLongQuery(t *testing.T) {
	sup := fastSup()
	sup.HungGrace = 40 * time.Millisecond
	spawns := &atomic.Int32{}
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: sup, RequestTimeout: 600 * time.Millisecond})
	c.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := c.StartSupervised(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	firstSpawn := spawns.Load()

	_ = c.RegisterPending(4242) // long-running analysis, no response yet
	// Simulate liveness: the worker streams progress during the computation.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
				c.lastActivity.Store(time.Now().UnixNano())
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // ≫ HungGrace, ≪ RequestTimeout

	if got := spawns.Load(); got != firstSpawn {
		t.Fatalf("legitimate long query was recycled %d times", got-firstSpawn)
	}
}
