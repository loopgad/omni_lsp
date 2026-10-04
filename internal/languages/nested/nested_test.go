package nested

// Supervision (§G2/F15), build-context identity (§E0), and request-wait
// tests for the shared nested-LSP bridge. All run against injected fake
// processes — no real language server required.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
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

type closeHandshakeWriter struct {
	stdout            *io.PipeWriter
	frames            chan jsonrpc.Message
	releaseShutdown   chan struct{}
	shutdownResponded chan struct{}
	respondShutdown   bool
	shutdownError     string
}

func (w *closeHandshakeWriter) Write(p []byte) (int, error) {
	frame := string(p)
	separator := strings.Index(frame, "\r\n\r\n")
	if separator < 0 {
		return 0, errors.New("malformed close handshake frame")
	}
	var message jsonrpc.Message
	if err := json.Unmarshal([]byte(frame[separator+4:]), &message); err != nil {
		return 0, err
	}
	w.frames <- message
	if message.Method == "shutdown" && w.respondShutdown {
		if w.releaseShutdown != nil {
			<-w.releaseShutdown
		}
		var responseMessage *jsonrpc.Message
		if w.shutdownError != "" {
			responseMessage = jsonrpc.NewErrorResponse(*message.ID, jsonrpc.RequestFailed, w.shutdownError, nil)
		} else {
			responseMessage = jsonrpc.NewResponse(*message.ID, json.RawMessage("null"))
		}
		response, err := json.Marshal(responseMessage)
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(w.stdout, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
			return 0, err
		}
		if w.shutdownResponded != nil {
			close(w.shutdownResponded)
		}
	}
	return len(p), nil
}

func (*closeHandshakeWriter) Close() error { return nil }

func TestClose_SendsShutdownRequestThenExitNotification(t *testing.T) {
	stdout, server := io.Pipe()
	writer := &closeHandshakeWriter{
		stdout: server, frames: make(chan jsonrpc.Message, 4),
		releaseShutdown: make(chan struct{}), shutdownResponded: make(chan struct{}), respondShutdown: true,
	}
	c := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	c.Attach(nil, writer, stdout)
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()

	var shutdown jsonrpc.Message
	select {
	case shutdown = <-writer.frames:
	case <-time.After(time.Second):
		t.Fatal("Close did not send shutdown")
	}
	if shutdown.Method != "shutdown" || shutdown.ID == nil {
		t.Fatalf("shutdown frame = %+v, want a shutdown request with an ID", shutdown)
	}
	select {
	case frame := <-writer.frames:
		t.Fatalf("sent %q before the shutdown response", frame.Method)
	case <-time.After(20 * time.Millisecond):
	}
	close(writer.releaseShutdown)

	select {
	case exit := <-writer.frames:
		if exit.Method != "exit" || exit.ID != nil {
			t.Fatalf("exit frame = %+v, want an exit notification", exit)
		}
		select {
		case <-writer.shutdownResponded:
		default:
			t.Fatal("exit notification was written before the shutdown response")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not send exit after receiving the shutdown response")
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("Close left %d pending request(s)", pending)
	}
	_ = server.Close()
}

func TestClose_ReportsShutdownTimeout(t *testing.T) {
	stdout, server := io.Pipe()
	writer := &closeHandshakeWriter{stdout: server, frames: make(chan jsonrpc.Message, 4)}
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	conn.Attach(nil, writer, stdout)

	started := time.Now()
	err := conn.Close()
	if err == nil || !strings.Contains(err.Error(), "shutdown request") {
		t.Fatalf("Close error = %v, want explicit shutdown timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close took %s after a 100ms fake-process deadline", elapsed)
	}
	shutdown := <-writer.frames
	if shutdown.Method != "shutdown" || shutdown.ID == nil {
		t.Fatalf("first frame = %+v, want shutdown request", shutdown)
	}
	select {
	case frame := <-writer.frames:
		t.Fatalf("sent %q despite shutdown timeout", frame.Method)
	default:
	}
	conn.mu.Lock()
	pending := len(conn.pending)
	conn.mu.Unlock()
	if pending != 0 {
		t.Fatalf("timed out Close left %d pending request(s)", pending)
	}
	_ = server.Close()
}

func TestClose_ReportsShutdownRefusalAfterExitNotification(t *testing.T) {
	stdout, server := io.Pipe()
	writer := &closeHandshakeWriter{
		stdout: server, frames: make(chan jsonrpc.Message, 4),
		respondShutdown: true, shutdownError: "server refused shutdown",
	}
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	conn.Attach(nil, writer, stdout)

	err := conn.Close()
	if err == nil || !strings.Contains(err.Error(), "server refused shutdown") {
		t.Fatalf("Close error = %v, want explicit refusal", err)
	}
	shutdown := <-writer.frames
	if shutdown.Method != "shutdown" || shutdown.ID == nil {
		t.Fatalf("first frame = %+v, want shutdown request", shutdown)
	}
	exit := <-writer.frames
	if exit.Method != "exit" || exit.ID != nil {
		t.Fatalf("second frame = %+v, want exit notification after refusal response", exit)
	}
	conn.mu.Lock()
	pending := len(conn.pending)
	conn.mu.Unlock()
	if pending != 0 {
		t.Fatalf("refused Close left %d pending request(s)", pending)
	}
	_ = server.Close()
}

const nestedCloseHelperEnv = "OMNILSP_NESTED_CLOSE_HELPER"
const nestedCloseIgnoreEnv = "OMNILSP_NESTED_CLOSE_IGNORE_SHUTDOWN"

func TestCloseWaitsForChildProcessExit(t *testing.T) {
	if marker := os.Getenv(nestedCloseHelperEnv); marker != "" {
		runNestedCloseHelper(t, marker)
		return
	}

	marker := filepath.Join(t.TempDir(), "shutdown-sequence.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloseWaitsForChildProcessExit$")
	cmd.Env = append(os.Environ(), nestedCloseHelperEnv+"="+marker)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child test process: %v", err)
	}
	conn := New(Config{Name: "child-lsp", Lang: "test", WorkDir: t.TempDir()})
	conn.Attach(cmd, stdin, stdout)

	closeResults := make(chan error, 2)
	go func() { closeResults <- conn.Close() }()
	go func() { closeResults <- conn.Close() }()
	for range 2 {
		select {
		case err := <-closeResults:
			if err != nil {
				t.Fatalf("concurrent Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not finish within the bounded shutdown interval")
		}
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("Close returned before the child process was reaped: state=%v", cmd.ProcessState)
	}
	sequence, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child did not record its shutdown sequence: %v", err)
	}
	if string(sequence) != "shutdown\nexit\n" {
		t.Fatalf("child shutdown sequence = %q, want shutdown then exit", sequence)
	}
}

func TestCloseForcedTerminationAfterShutdownTimeoutReportsLifecycleFailure(t *testing.T) {
	if marker := os.Getenv(nestedCloseIgnoreEnv); marker != "" {
		runNestedShutdownTimeoutHelper(t, marker)
		return
	}

	marker := filepath.Join(t.TempDir(), "shutdown-request-received.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloseForcedTerminationAfterShutdownTimeoutReportsLifecycleFailure$")
	cmd.Env = append(os.Environ(), nestedCloseIgnoreEnv+"="+marker)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child test process: %v", err)
	}
	conn := New(Config{Name: "stuck-lsp", Lang: "test", WorkDir: t.TempDir()})
	conn.Attach(cmd, stdin, stdout)

	started := time.Now()
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err == nil || !strings.Contains(err.Error(), "shutdown request") {
			t.Fatalf("Close error = %v, want shutdown-timeout lifecycle failure", err)
		}
	case <-time.After(shutdownGracePeriod + forcedExitWait + closeLeaseWait + 3*time.Second):
		t.Fatal("Close exceeded graceful plus forced shutdown bounds")
	}
	if elapsed := time.Since(started); elapsed > shutdownGracePeriod+forcedExitWait+closeLeaseWait+3*time.Second {
		t.Fatalf("Close took %s, beyond the bounded shutdown interval", elapsed)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("Close returned before the child process was reaped: state=%v", cmd.ProcessState)
	}
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child did not observe shutdown request: %v", err)
	}
	if string(markerData) != "shutdown\n" {
		t.Fatalf("child marker = %q, want shutdown request", markerData)
	}
}

func runNestedCloseHelper(t *testing.T, marker string) {
	t.Helper()
	reader := bufio.NewReader(os.Stdin)
	codec := jsonrpc.NewCodec()
	shutdownSeen := false
	for {
		message, err := codec.ReadMessage(reader)
		if err != nil {
			t.Fatalf("read child LSP message: %v", err)
		}
		switch message.Method {
		case "shutdown":
			if message.ID == nil {
				t.Fatal("shutdown arrived as a notification")
			}
			shutdownSeen = true
			if err := codec.WriteMessage(os.Stdout, jsonrpc.NewResponse(*message.ID, json.RawMessage("null"))); err != nil {
				t.Fatalf("write shutdown response: %v", err)
			}
		case "exit":
			if !shutdownSeen {
				t.Fatal("exit arrived before shutdown request")
			}
			if err := os.WriteFile(marker, []byte("shutdown\nexit\n"), 0o600); err != nil {
				t.Fatalf("write sequence marker: %v", err)
			}
			time.Sleep(150 * time.Millisecond)
			return
		default:
			t.Fatalf("unexpected child LSP message %q", message.Method)
		}
	}
}

func runNestedShutdownTimeoutHelper(t *testing.T, marker string) {
	t.Helper()
	message, err := jsonrpc.NewCodec().ReadMessage(bufio.NewReader(os.Stdin))
	if err != nil {
		t.Fatalf("read shutdown request: %v", err)
	}
	if message.Method != "shutdown" || message.ID == nil {
		t.Fatalf("received %+v, want shutdown request", message)
	}
	if err := os.WriteFile(marker, []byte("shutdown\n"), 0o600); err != nil {
		t.Fatalf("record shutdown request: %v", err)
	}
	select {}
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

type notificationRecorder struct{ frames chan string }

func newNotificationRecorder() *notificationRecorder {
	return &notificationRecorder{frames: make(chan string, 8)}
}

func (r *notificationRecorder) Write(p []byte) (int, error) {
	r.frames <- string(p)
	return len(p), nil
}

func (r *notificationRecorder) Close() error { return nil }

type failingNotificationRecorder struct{ err error }

func (r failingNotificationRecorder) Write([]byte) (int, error) { return 0, r.err }
func (failingNotificationRecorder) Close() error                { return nil }

type initializeResponseWriter struct {
	stdout *io.PipeWriter
	frames chan string
}

func (w *initializeResponseWriter) Write(p []byte) (int, error) {
	frame := string(p)
	w.frames <- frame
	separator := strings.Index(frame, "\r\n\r\n")
	if separator < 0 {
		return 0, errors.New("malformed initialize request frame")
	}
	var request jsonrpc.Message
	if err := json.Unmarshal([]byte(frame[separator+4:]), &request); err != nil {
		return 0, err
	}
	if request.Method != "initialize" || request.ID == nil {
		return len(p), nil
	}
	raw, err := json.Marshal(jsonrpc.NewResponse(*request.ID, json.RawMessage(`{}`)))
	if err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(w.stdout, "Content-Length: %d\r\n\r\n%s", len(raw), raw); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *initializeResponseWriter) Close() error { return nil }

func TestInitializeAdvertisesDiagnosticsOnlyWhenCaptured(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%t", capture), func(t *testing.T) {
			stdout, server := io.Pipe()
			writer := &initializeResponseWriter{stdout: server, frames: make(chan string, 4)}
			conn := New(Config{
				Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
				RequestTimeout: time.Second, CaptureDiagnostics: capture,
			})
			conn.Attach(nil, writer, stdout)
			t.Cleanup(func() {
				_ = conn.Close()
				_ = server.Close()
			})
			if err := conn.Initialize(); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			request := decodeRecordedNotification(t, <-writer.frames)
			if request.Method != "initialize" {
				t.Fatalf("first request method = %q, want initialize", request.Method)
			}
			var params struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatalf("decode initialize params: %v", err)
			}
			if !capture {
				if _, exists := params.Capabilities["textDocument"]; exists {
					t.Fatal("diagnostics capability was advertised while capture was disabled")
				}
				return
			}
			var textDocument struct {
				PublishDiagnostics struct {
					RelatedInformation bool `json:"relatedInformation"`
				} `json:"publishDiagnostics"`
			}
			if err := json.Unmarshal(params.Capabilities["textDocument"], &textDocument); err != nil {
				t.Fatalf("decode textDocument capabilities: %v", err)
			}
			if !textDocument.PublishDiagnostics.RelatedInformation {
				t.Fatal("textDocument.publishDiagnostics.relatedInformation was not advertised")
			}
		})
	}
}

func newDiagnosticsConn(t *testing.T, timeout time.Duration) (*Conn, *io.PipeWriter, *notificationRecorder) {
	t.Helper()
	reader, writer := io.Pipe()
	recorder := newNotificationRecorder()
	conn := New(Config{
		Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
		RequestTimeout: timeout, CaptureDiagnostics: true,
	})
	conn.Attach(nil, recorder, reader)
	t.Cleanup(func() {
		conn.Close()
		_ = writer.Close()
	})
	return conn, writer, recorder
}

func writeLSPNotification(t *testing.T, writer io.Writer, method string, params any) {
	t.Helper()
	rawParams, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	message := &jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: method, Params: rawParams}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
		t.Fatal(err)
	}
}

func decodeRecordedNotification(t *testing.T, frame string) jsonrpc.Message {
	t.Helper()
	separator := strings.Index(frame, "\r\n\r\n")
	if separator < 0 {
		t.Fatalf("malformed outgoing LSP frame %q", frame)
	}
	var message jsonrpc.Message
	if err := json.Unmarshal([]byte(frame[separator+4:]), &message); err != nil {
		t.Fatalf("decode outgoing LSP message: %v", err)
	}
	return message
}

func TestNestedDocumentSyncUsesMonotonicVersions(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.py"

	first, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 1\n"), 10)
	if err != nil {
		t.Fatal(err)
	}
	open := decodeRecordedNotification(t, <-recorder.frames)
	if open.Method != "textDocument/didOpen" || first.Version != 1 {
		t.Fatalf("first sync = method %q, version %d; want didOpen v1", open.Method, first.Version)
	}

	same, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 1\n"), 10)
	if err != nil || same.Version != first.Version || same.serial != first.serial {
		t.Fatalf("unchanged sync = %+v, %v; want same generation", same, err)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("unchanged content sent another notification: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}

	second, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 2\n"), 11)
	if err != nil || second.Version != 2 {
		t.Fatalf("changed sync = %+v, %v; want version 2", second, err)
	}
	change := decodeRecordedNotification(t, <-recorder.frames)
	if change.Method != "textDocument/didChange" {
		t.Fatalf("changed content method = %q, want didChange", change.Method)
	}
	var changeParams struct {
		TextDocument struct {
			Version int32 `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(change.Params, &changeParams); err != nil {
		t.Fatal(err)
	}
	if changeParams.TextDocument.Version != 2 || len(changeParams.ContentChanges) != 1 || changeParams.ContentChanges[0].Text != "value = 2\n" {
		t.Fatalf("didChange params = %+v", changeParams)
	}

	if _, err := conn.SyncDocumentAtRevision("python", uri, []byte("stale\n"), 10); err == nil {
		t.Fatal("older snapshot revision was allowed to replace newer document content")
	}
}

func TestDidSaveDocumentAtRevisionSyncsOpenDocumentBeforeSave(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.rs"
	const updatedContent = "fn main() { missing_name; }\n"

	if _, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() {}\n"), 10); err != nil {
		t.Fatal(err)
	}
	if open := decodeRecordedNotification(t, <-recorder.frames); open.Method != "textDocument/didOpen" {
		t.Fatalf("initial notification = %q, want didOpen", open.Method)
	}
	if err := conn.DidSaveDocumentAtRevision("rust", uri, []byte(updatedContent), 11); err != nil {
		t.Fatalf("save updated document: %v", err)
	}
	change := decodeRecordedNotification(t, <-recorder.frames)
	if change.Method != "textDocument/didChange" {
		t.Fatalf("first save notification = %q, want didChange before didSave", change.Method)
	}
	var changeParams struct {
		TextDocument struct {
			Version int32 `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(change.Params, &changeParams); err != nil {
		t.Fatal(err)
	}
	if changeParams.TextDocument.Version != 2 || len(changeParams.ContentChanges) != 1 || changeParams.ContentChanges[0].Text != updatedContent {
		t.Fatalf("didChange payload = %+v; want current content at child version 2", changeParams)
	}
	save := decodeRecordedNotification(t, <-recorder.frames)
	if save.Method != "textDocument/didSave" {
		t.Fatalf("second save notification = %q, want didSave", save.Method)
	}
	var saveParams struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(save.Params, &saveParams); err != nil {
		t.Fatal(err)
	}
	if saveParams.TextDocument.URI != uri || saveParams.Text != "" {
		t.Fatalf("didSave payload = %+v; want the opened URI without an unnegotiated text copy", saveParams)
	}

	// Same content at a later save revision should not duplicate didChange or
	// advance the child document version; it still forwards the save event.
	if err := conn.DidSaveDocumentAtRevision("rust", uri, []byte(updatedContent), 12); err != nil {
		t.Fatalf("save unchanged document: %v", err)
	}
	save = decodeRecordedNotification(t, <-recorder.frames)
	if save.Method != "textDocument/didSave" {
		t.Fatalf("unchanged save notification = %q, want didSave without a duplicate didChange", save.Method)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("unchanged save sent an extra notification: %s", frame)
	default:
	}

	if err := conn.DidSaveDocumentAtRevision("rust", "file:///workspace/not-open.rs", []byte("stale\n"), 13); err != nil {
		t.Fatalf("save unopened document: %v", err)
	}
	if err := conn.DidSaveDocumentAtRevision("rust", uri, []byte("stale\n"), 11); err == nil {
		t.Fatal("older save snapshot was accepted")
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("unopened or stale save sent a child notification: %s", frame)
	default:
	}
}

func TestNestedDiagnosticsIgnoreStaleAndUnversionedUpdates(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.rs"
	first, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() {}\n"), 20)
	if err != nil {
		t.Fatal(err)
	}
	oldResult := make(chan error, 1)
	go func() {
		_, err := conn.WaitForDiagnostics(context.Background(), first)
		oldResult <- err
	}()
	second, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() { missing(); }\n"), 21)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-oldResult:
		if err == nil {
			t.Fatal("old waiter returned diagnostics after the document advanced")
		}
	case <-time.After(time.Second):
		t.Fatal("document update stranded the old diagnostics waiter")
	}

	currentResult := make(chan []Diagnostic, 1)
	currentError := make(chan error, 1)
	go func() {
		items, err := conn.WaitForDiagnostics(context.Background(), second)
		currentResult <- items
		currentError <- err
	}()
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": first.Version,
		"diagnostics": []any{map[string]any{
			"range":   map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
			"message": "stale diagnostic",
		}},
	})
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri":         uri,
		"diagnostics": []any{},
	})
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": second.Version,
		"diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 11}, "end": map[string]any{"line": 0, "character": 18}},
			"severity": 1, "code": 17, "source": "rust-analyzer", "message": "cannot find function `missing`",
		}},
	})

	select {
	case err := <-currentError:
		if err != nil {
			t.Fatalf("wait current diagnostics: %v", err)
		}
		items := <-currentResult
		if len(items) != 1 || items[0].Message != "cannot find function `missing`" {
			t.Fatalf("current diagnostics = %+v", items)
		}
		if items[0].Code != "17" || items[0].Source != "rust-analyzer" || items[0].StartChar != 11 || items[0].EndChar != 18 {
			t.Fatalf("normalized diagnostic = %+v", items[0])
		}
	case <-time.After(time.Second):
		t.Fatal("current diagnostics notification was not forwarded")
	}
}

func TestNestedDiagnosticsTimeoutIsAnError(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, 30*time.Millisecond)
	token, err := conn.SyncDocumentAtRevision("typescript", "file:///workspace/main.ts", []byte("const x = 1;\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	items, err := conn.WaitForDiagnostics(context.Background(), token)
	if err == nil || items != nil {
		t.Fatalf("missing diagnostics = (%v, %v), want error and no successful empty result", items, err)
	}
}

func TestNestedDiagnosticsWaitHonorsCancellation(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, time.Second)
	token, err := conn.SyncDocumentAtRevision("typescript", "file:///workspace/cancel.ts", []byte("const x = 1;\n"), 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.WaitForDiagnostics(ctx, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled diagnostics wait error = %v, want context.Canceled", err)
	}
}

func TestNestedDiagnosticsWaiterFailsOnWorkerRestart(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	token, err := conn.SyncDocumentAtRevision("rust", "file:///workspace/restart.rs", []byte("fn main() {}\n"), 9)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // initial didOpen
	result := make(chan error, 1)
	go func() {
		_, err := conn.WaitForDiagnostics(context.Background(), token)
		result <- err
	}()

	stdout, replacementWriter := io.Pipe()
	replacementRecorder := newNotificationRecorder()
	conn.Attach(nil, replacementRecorder, stdout)
	t.Cleanup(func() { _ = replacementWriter.Close() })
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("restart waiter completed without the old worker diagnostics")
		}
	case <-time.After(time.Second):
		t.Fatal("worker restart stranded the diagnostics waiter")
	}
	if _, err := conn.WaitForDiagnostics(context.Background(), token); err == nil {
		t.Fatal("old worker token remained valid after restart")
	}
	if _, err := conn.SyncDocumentAtRevision("rust", token.URI, []byte("stale\n"), 8); err == nil {
		t.Fatal("restart discarded the last snapshot revision and accepted stale content")
	}
	reopened, err := conn.SyncDocumentAtRevision("rust", token.URI, []byte("fn main() {}\n"), 9)
	if err != nil || reopened.Version != token.Version+1 {
		t.Fatalf("current snapshot after restart = %+v, %v; want a higher child version", reopened, err)
	}
	var reopen jsonrpc.Message
	select {
	case frame := <-replacementRecorder.frames:
		reopen = decodeRecordedNotification(t, frame)
	case <-time.After(time.Second):
		t.Fatal("restart did not reopen the current document")
	}
	if reopen.Method != "textDocument/didOpen" {
		t.Fatalf("restart resync method = %q, want didOpen", reopen.Method)
	}
	writeLSPNotification(t, replacementWriter, "textDocument/publishDiagnostics", map[string]any{
		"uri": token.URI, "diagnostics": []any{},
	})
	if items, err := conn.WaitForDiagnostics(context.Background(), reopened); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("versionless report after worker restart = (%v, %v), want accepted empty report", items, err)
	}
}

func TestNestedDidCloseRejectsOldSnapshotsAndReopensNewer(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/closed.rs"
	first, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() {}\n"), 40)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // initial didOpen
	waiter := make(chan error, 1)
	go func() {
		_, err := conn.WaitForDiagnostics(context.Background(), first)
		waiter <- err
	}()
	if err := conn.CloseDocument(uri, 40); err != nil {
		t.Fatal(err)
	}
	closeNotice := decodeRecordedNotification(t, <-recorder.frames)
	if closeNotice.Method != "textDocument/didClose" {
		t.Fatalf("close notification = %q, want didClose", closeNotice.Method)
	}
	select {
	case err := <-waiter:
		if err == nil {
			t.Fatal("didClose did not invalidate the in-flight diagnostics waiter")
		}
	case <-time.After(time.Second):
		t.Fatal("didClose stranded the diagnostics waiter")
	}
	for _, staleRevision := range []uint64{39, 40} {
		if _, err := conn.SyncDocumentAtRevision("rust", uri, []byte("stale\n"), staleRevision); err == nil {
			t.Fatalf("didClose accepted stale snapshot revision %d", staleRevision)
		}
	}
	reopened, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() { }\n"), 41)
	if err != nil || reopened.Version != first.Version+1 {
		t.Fatalf("newer snapshot reopen = %+v, %v; want didOpen with a higher version", reopened, err)
	}
	openNotice := decodeRecordedNotification(t, <-recorder.frames)
	if openNotice.Method != "textDocument/didOpen" {
		t.Fatalf("reopen notification = %q, want didOpen", openNotice.Method)
	}
}

func TestNestedDiagnosticsAcceptUnversionedInitialReport(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	token, err := conn.SyncDocumentAtRevision("typescript", "file:///workspace/initial.ts", []byte("let x = 1;\n"), 7)
	if err != nil {
		t.Fatal(err)
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": token.URI, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 4}, "end": map[string]any{"line": 0, "character": 5}},
			"severity": 1, "code": 2304, "source": "typescript", "message": "Cannot find name 'missing'.",
		}},
	})
	items, err := conn.WaitForDiagnostics(context.Background(), token)
	if err != nil || len(items) != 1 || items[0].Code != "2304" || items[0].Source != "typescript" {
		t.Fatalf("initial TSLS-style versionless report = (%v, %v), want current TS2304 diagnostic", items, err)
	}
}

func TestNestedInitialDiagnosticsWaitsForRuntimeUpdateCallback(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/callback-order.rs"
	token, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() { missing(); }\n"), 8)
	if err != nil {
		t.Fatal(err)
	}
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	defer release()
	conn.SetDiagnosticsUpdateHandler(func(got string) {
		if got != uri {
			t.Errorf("diagnostics callback URI = %q, want %q", got, uri)
		}
		close(callbackStarted)
		<-releaseCallback
	})

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
			"uri": uri, "version": token.Version, "diagnostics": []any{map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 11}, "end": map[string]any{"line": 0, "character": 18}},
				"severity": 1, "code": "E0425", "source": "rust-analyzer", "message": "cannot find function `missing`",
			}},
		})
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime diagnostics callback did not start")
	}

	waitDone := make(chan error, 1)
	go func() {
		items, err := conn.WaitForDiagnostics(context.Background(), token)
		if err == nil && (len(items) != 1 || items[0].Message != "cannot find function `missing`") {
			err = fmt.Errorf("wait returned diagnostics %+v", items)
		}
		waitDone <- err
	}()
	select {
	case err := <-waitDone:
		t.Fatalf("diagnostics waiter escaped before runtime callback completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("wait diagnostics: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostics waiter did not resume after callback completed")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("diagnostics notification writer did not finish")
	}
}

func TestNestedDiagnosticsMatchCanonicalURIAndPreserveTrackedSpelling(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	const trackedURI = "file:///D:/workspace/main.py"
	const childURI = "file:///d%3A/workspace/main.py"
	token, err := conn.SyncDocumentAtRevision("python", trackedURI, []byte("missing_name\n"), 19)
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan string, 1)
	conn.SetDiagnosticsUpdateHandler(func(uri string) { updates <- uri })
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": childURI, "version": token.Version, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 12}},
			"severity": 1, "code": "reportUndefinedVariable", "source": "pyright", "message": "missing_name is not defined",
		}},
	})
	select {
	case got := <-updates:
		if got != trackedURI {
			t.Fatalf("diagnostic update URI = %q, want original spelling %q", got, trackedURI)
		}
	case <-time.After(time.Second):
		t.Fatal("canonical-equivalent child URI was not captured")
	}
	items, err := conn.WaitForDiagnostics(context.Background(), token)
	if err != nil || len(items) != 1 || items[0].Message != "missing_name is not defined" {
		t.Fatalf("canonical-equivalent diagnostics = (%+v, %v)", items, err)
	}
}

func TestNestedCanonicalURIReopenCarriesVersionHighWaterAndResolvesActiveAlias(t *testing.T) {
	conn, writer, recorder := newDiagnosticsConn(t, time.Second)
	const firstURI = "file:///D:/workspace/reopened.py"
	const reopenedURI = "file:///d%3A/workspace/reopened.py"
	const childURI = "file:///d:/workspace/reopened.py"
	content := []byte("missing_name\n")
	first, err := conn.SyncDocumentAtRevision("python", firstURI, content, 40)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // first didOpen
	if err := conn.CloseDocument(firstURI, 41); err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // didClose
	reopened, err := conn.SyncDocumentAtRevision("python", reopenedURI, content, 42)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Version != first.Version+1 {
		t.Fatalf("reopened child version = %d, want high-water version %d", reopened.Version, first.Version+1)
	}
	_ = <-recorder.frames // reopened didOpen

	updates := make(chan string, 2)
	conn.SetDiagnosticsUpdateHandler(func(uri string) { updates <- uri })
	params := func(version int32, message string) map[string]any {
		return map[string]any{
			"uri": childURI, "version": version, "diagnostics": []any{map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 12}},
				"severity": 1, "code": "reportUndefinedVariable", "source": "pyright", "message": message,
			}},
		}
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", params(first.Version, "stale first-open result"))
	select {
	case got := <-updates:
		t.Fatalf("stale prior-open report triggered update for %q", got)
	case <-time.After(30 * time.Millisecond):
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", params(reopened.Version, "current reopened result"))
	select {
	case got := <-updates:
		if got != reopenedURI {
			t.Fatalf("reopened diagnostic update URI = %q, want %q", got, reopenedURI)
		}
	case <-time.After(time.Second):
		t.Fatal("canonical child URI did not resolve to the active reopened spelling")
	}
	items, err := conn.WaitForDiagnostics(context.Background(), reopened)
	if err != nil || len(items) != 1 || items[0].Message != "current reopened result" {
		t.Fatalf("reopened diagnostics = (%+v, %v)", items, err)
	}
}

func TestNestedCanonicalURIHistorySurvivesAliasRoundTrip(t *testing.T) {
	conn, writer, recorder := newDiagnosticsConn(t, time.Second)
	const uriA = "file:///D:/workspace/roundtrip.py"
	const uriB = "file:///d%3A/workspace/roundtrip.py"
	const childURI = "file:///d:/workspace/roundtrip.py"
	content := []byte("missing_name\n")

	first, err := conn.SyncDocumentAtRevision("python", uriA, content, 50)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // A didOpen
	if err := conn.CloseDocument(uriA, 51); err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // A didClose
	second, err := conn.SyncDocumentAtRevision("python", uriB, content, 52)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != first.Version+1 {
		t.Fatalf("B child version = %d, want %d", second.Version, first.Version+1)
	}
	_ = <-recorder.frames // B didOpen
	if err := conn.CloseDocument(uriB, 53); err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // B didClose
	third, err := conn.SyncDocumentAtRevision("python", uriA, content, 54)
	if err != nil {
		t.Fatal(err)
	}
	if third.Version != second.Version+1 {
		t.Fatalf("A child version after A→B→A = %d, want %d", third.Version, second.Version+1)
	}
	_ = <-recorder.frames // A didOpen

	updates := make(chan string, 2)
	conn.SetDiagnosticsUpdateHandler(func(got string) { updates <- got })
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": childURI, "version": second.Version, "diagnostics": []any{map[string]any{
			"range":   map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 12}},
			"message": "stale alias report",
		}},
	})
	select {
	case got := <-updates:
		t.Fatalf("stale B report triggered update for %q", got)
	case <-time.After(30 * time.Millisecond):
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": childURI, "version": third.Version, "diagnostics": []any{map[string]any{
			"range":   map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 12}},
			"message": "current A report",
		}},
	})
	select {
	case got := <-updates:
		if got != uriA {
			t.Fatalf("active alias update URI = %q, want %q", got, uriA)
		}
	case <-time.After(time.Second):
		t.Fatal("current A report did not resolve through the canonical URI")
	}
	items, err := conn.WaitForDiagnostics(context.Background(), third)
	if err != nil || len(items) != 1 || items[0].Message != "current A report" {
		t.Fatalf("A→B→A diagnostics = (%+v, %v)", items, err)
	}
}

func TestNestedCloseDocumentResolvesEquivalentActiveURI(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const openedURI = "file:///D:/workspace/close-alias.py"
	const closeURI = "file:///d%3A/workspace/close-alias.py"
	content := []byte("value = 1\n")
	first, err := conn.SyncDocumentAtRevision("python", openedURI, content, 60)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // didOpen
	if err := conn.CloseDocument(closeURI, 61); err != nil {
		t.Fatal(err)
	}
	closeMessage := decodeRecordedNotification(t, <-recorder.frames)
	if closeMessage.Method != "textDocument/didClose" {
		t.Fatalf("close method = %q, want didClose", closeMessage.Method)
	}
	var closeParams struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(closeMessage.Params, &closeParams); err != nil {
		t.Fatal(err)
	}
	if closeParams.TextDocument.URI != openedURI {
		t.Fatalf("child didClose URI = %q, want tracked spelling %q", closeParams.TextDocument.URI, openedURI)
	}
	if _, state := conn.findDocumentURI(closeURI); state != nil {
		t.Fatal("equivalent close left the tracked document active")
	}
	reopened, err := conn.SyncDocumentAtRevision("python", closeURI, content, 62)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Version != first.Version+1 {
		t.Fatalf("reopened alias version = %d, want %d", reopened.Version, first.Version+1)
	}
	openMessage := decodeRecordedNotification(t, <-recorder.frames)
	if openMessage.Method != "textDocument/didOpen" {
		t.Fatalf("alias reopen method = %q, want didOpen", openMessage.Method)
	}
}

func TestNestedFailedDocumentNotificationRetainsVersionHighWater(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/write-failure.py"
	first, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 1\n"), 70)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // didOpen v1

	writeErr := errors.New("synthetic child stdin failure")
	conn.mu.Lock()
	conn.stdin = failingNotificationRecorder{err: writeErr}
	conn.mu.Unlock()
	if _, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 2\n"), 71); !errors.Is(err, writeErr) {
		t.Fatalf("failed didChange error = %v, want %v", err, writeErr)
	}

	retryRecorder := newNotificationRecorder()
	conn.mu.Lock()
	conn.stdin = retryRecorder
	conn.mu.Unlock()
	reopened, err := conn.SyncDocumentAtRevision("python", uri, []byte("value = 2\n"), 72)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Version != first.Version+2 {
		t.Fatalf("reopen version after failed didChange = %d, want %d", reopened.Version, first.Version+2)
	}
	message := decodeRecordedNotification(t, <-retryRecorder.frames)
	if message.Method != "textDocument/didOpen" {
		t.Fatalf("retry method = %q, want didOpen", message.Method)
	}
}

func TestNestedSameVersionReplacementWaitsForRuntimeInvalidationCallback(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/callback-replacement.rs"
	token, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() { missing(); }\n"), 31)
	if err != nil {
		t.Fatal(err)
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": token.Version, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 11}, "end": map[string]any{"line": 0, "character": 18}},
			"severity": 1, "code": "E0", "source": "rust-analyzer", "message": "older report",
		}},
	})
	first, err := conn.WaitForDiagnostics(context.Background(), token)
	if err != nil || len(first) != 1 || first[0].Message != "older report" {
		t.Fatalf("initial diagnostics = (%+v, %v)", first, err)
	}

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	conn.SetDiagnosticsUpdateHandler(func(got string) {
		if got != uri {
			t.Errorf("diagnostics callback URI = %q, want %q", got, uri)
		}
		close(callbackStarted)
		<-releaseCallback
	})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
			"uri": uri, "version": token.Version, "diagnostics": []any{map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 11}, "end": map[string]any{"line": 0, "character": 18}},
				"severity": 1, "code": "E1", "source": "rust-analyzer", "message": "replacement report",
			}},
		})
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime diagnostics callback did not start for the replacement")
	}

	waitDone := make(chan struct {
		items []Diagnostic
		err   error
	}, 1)
	go func() {
		items, err := conn.WaitForDiagnostics(context.Background(), token)
		waitDone <- struct {
			items []Diagnostic
			err   error
		}{items: items, err: err}
	}()
	select {
	case got := <-waitDone:
		t.Fatalf("wait returned before runtime invalidation completed: (%+v, %v)", got.items, got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseCallback)
	select {
	case got := <-waitDone:
		if got.err != nil || len(got.items) != 1 || got.items[0].Message != "replacement report" {
			t.Fatalf("replacement diagnostics = (%+v, %v)", got.items, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostics waiter did not return after runtime invalidation completed")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("replacement diagnostic notification writer did not finish")
	}
}

func TestNestedDiagnosticsAllowUnversionedCurrentTypescriptAndRejectExplicitStaleVersion(t *testing.T) {
	conn, writer, recorder := newDiagnosticsConn(t, time.Second)
	conn.cfg.AllowUnversionedDiagnostics = true
	updates := make(chan string, 2)
	conn.SetDiagnosticsUpdateHandler(func(uri string) { updates <- uri })
	const uri = "file:///workspace/current.ts"
	first, err := conn.SyncDocumentAtRevision("typescript", uri, []byte("let missing = 1;\n"), 20)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // initial didOpen
	current, err := conn.SyncDocumentAtRevision("typescript", uri, []byte("let missing = missingName;\n"), 21)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames // current didChange

	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": first.Version, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 4}, "end": map[string]any{"line": 0, "character": 11}},
			"severity": 1, "code": 2304, "source": "typescript", "message": "stale report",
		}},
	})
	select {
	case got := <-updates:
		t.Fatalf("explicit stale version triggered diagnostic update for %s", got)
	case <-time.After(50 * time.Millisecond):
	}

	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 14}, "end": map[string]any{"line": 0, "character": 26}},
			"severity": 1, "code": 2304, "source": "typescript", "message": "Cannot find name 'missingName'.",
		}},
	})
	select {
	case got := <-updates:
		if got != uri {
			t.Fatalf("diagnostic update URI = %q, want %q", got, uri)
		}
	case <-time.After(time.Second):
		t.Fatal("current TSLS-style report was not accepted")
	}
	items, err := conn.WaitForDiagnostics(context.Background(), current)
	if err != nil || len(items) != 1 || items[0].Message != "Cannot find name 'missingName'." {
		t.Fatalf("current versionless diagnostics = (%+v, %v)", items, err)
	}
}

func TestNestedDiagnosticsSameVersionUpdateReplacesInitialEmptyReport(t *testing.T) {
	conn, writer, _ := newDiagnosticsConn(t, time.Second)
	updates := make(chan string, 2)
	conn.SetDiagnosticsUpdateHandler(func(uri string) { updates <- uri })
	const uri = "file:///workspace/streaming.rs"
	token, err := conn.SyncDocumentAtRevision("rust", uri, []byte("fn main() { missing(); }\n"), 30)
	if err != nil {
		t.Fatal(err)
	}
	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": token.Version, "diagnostics": []any{},
	})
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("initial empty diagnostic set was not received")
	}
	if items, err := conn.WaitForDiagnostics(context.Background(), token); err != nil || len(items) != 0 {
		t.Fatalf("initial same-version diagnostics = (%+v, %v), want empty set", items, err)
	}

	writeLSPNotification(t, writer, "textDocument/publishDiagnostics", map[string]any{
		"uri": uri, "version": token.Version, "diagnostics": []any{map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 11}, "end": map[string]any{"line": 0, "character": 18}},
			"severity": 1, "code": "E0425", "source": "rust-analyzer", "message": "cannot find function `missing`",
		}},
	})
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("later same-version diagnostics did not replace the initial report")
	}
	items, err := conn.WaitForDiagnostics(context.Background(), token)
	if err != nil || len(items) != 1 || items[0].Message != "cannot find function `missing`" {
		t.Fatalf("latest same-version diagnostics = (%+v, %v)", items, err)
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

type requestTimingTestWriter struct {
	stdout        *io.PipeWriter
	requests      chan jsonrpc.Message
	respond       bool
	writeDelay    time.Duration
	responseDelay time.Duration
}

func (w *requestTimingTestWriter) Write(p []byte) (int, error) {
	if w.writeDelay > 0 {
		time.Sleep(w.writeDelay)
	}
	frame := string(p)
	separator := strings.Index(frame, "\r\n\r\n")
	if separator < 0 {
		return 0, errors.New("malformed request timing frame")
	}
	var request jsonrpc.Message
	if err := json.Unmarshal([]byte(frame[separator+4:]), &request); err != nil {
		return 0, err
	}
	if w.requests != nil {
		w.requests <- request
	}
	if w.respond && request.ID != nil {
		data, err := json.Marshal(jsonrpc.NewResponse(*request.ID, json.RawMessage(`null`)))
		if err != nil {
			return 0, err
		}
		if w.responseDelay > 0 {
			go func() {
				time.Sleep(w.responseDelay)
				_, _ = fmt.Fprintf(w.stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
			}()
		} else if _, err := fmt.Fprintf(w.stdout, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *requestTimingTestWriter) Close() error { return w.stdout.Close() }

func assertRequestTimingSamplesHavePhaseFields(t *testing.T, data []byte, wantSamples int) {
	t.Helper()
	var trace struct {
		Samples []map[string]json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("decode raw timing samples: %v", err)
	}
	if len(trace.Samples) != wantSamples {
		t.Fatalf("raw timing samples = %d, want %d", len(trace.Samples), wantSamples)
	}
	for i, sample := range trace.Samples {
		for _, field := range []string{"write_duration_ns", "wait_duration_ns"} {
			if _, ok := sample[field]; !ok {
				t.Errorf("sample %d is missing %q", i, field)
			}
		}
	}
}

func TestNestedRPCTimingCppMethodsAggregateAndFlush(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)

	stdout, server := io.Pipe()
	writer := &requestTimingTestWriter{stdout: server, respond: true}
	conn := New(Config{Lang: "cpp", RequestTimeout: time.Second})
	if conn.requestTiming == nil {
		t.Fatal("cpp connection did not enable timing from the configured path")
	}
	conn.Attach(nil, writer, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = server.Close()
	})

	const documentURI = "file:///workspace/main.cpp"
	for _, method := range []string{
		"textDocument/completion",
		"textDocument/completion",
		"textDocument/documentSymbol",
		"textDocument/hover",
	} {
		params := map[string]any{"textDocument": map[string]string{"uri": documentURI}}
		if _, err := conn.SendRequest(context.Background(), method, params); err != nil {
			t.Fatalf("SendRequest(%q): %v", method, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read timing trace at configured path: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode timing trace: %v", err)
	}
	assertRequestTimingSamplesHavePhaseFields(t, data, 3)
	if report.Version != 1 || report.Language != "cpp" {
		t.Fatalf("trace header = version %d language %q", report.Version, report.Language)
	}
	if len(report.Aggregates) != 2 {
		t.Fatalf("aggregates = %+v, want only completion and documentSymbol", report.Aggregates)
	}
	byMethod := make(map[string]requestTimingAggregate, len(report.Aggregates))
	for _, aggregate := range report.Aggregates {
		byMethod[aggregate.Method] = aggregate
		if aggregate.URI != documentURI {
			t.Errorf("aggregate URI = %q, want %q", aggregate.URI, documentURI)
		}
	}
	completion := byMethod["textDocument/completion"]
	if completion.Count != 2 || completion.Responses != 2 {
		t.Errorf("completion aggregate = %+v, want two responses", completion)
	}
	symbols := byMethod["textDocument/documentSymbol"]
	if symbols.Count != 1 || symbols.Responses != 1 {
		t.Errorf("documentSymbol aggregate = %+v, want one response", symbols)
	}
	if len(report.Samples) != 3 {
		t.Fatalf("samples = %+v, want only the three selected RPCs", report.Samples)
	}
	for _, sample := range report.Samples {
		if sample.URI != documentURI || sample.Outcome != string(requestTimingResponse) {
			t.Errorf("sample = %+v, want response for %q", sample, documentURI)
		}
	}
}

func TestNestedRPCTimingSampleSeparatesWriteAndWaitDurations(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)
	stdout, server := io.Pipe()
	writer := &requestTimingTestWriter{
		stdout: server, respond: true,
		writeDelay: 30 * time.Millisecond, responseDelay: 30 * time.Millisecond,
	}
	conn := New(Config{Lang: "cpp", RequestTimeout: time.Second})
	conn.Attach(nil, writer, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = server.Close()
	})

	const documentURI = "file:///workspace/timing.cpp"
	params := map[string]any{"textDocument": map[string]string{"uri": documentURI}}
	if _, err := conn.SendRequest(context.Background(), "textDocument/completion", params); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	// The sampled request is complete. Do not apply artificial RPC delays to
	// the separate 100 ms graceful-exit deadline under parallel test load.
	writer.writeDelay = 0
	writer.responseDelay = 0
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read timing trace: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode timing trace: %v", err)
	}
	if len(report.Samples) != 1 {
		t.Fatalf("samples = %+v, want one completion sample", report.Samples)
	}
	sample := report.Samples[0]
	if sample.WriteDuration < uint64((20 * time.Millisecond).Nanoseconds()) {
		t.Errorf("write duration = %s, want at least 20ms", time.Duration(sample.WriteDuration))
	}
	if sample.WaitDuration < uint64((20 * time.Millisecond).Nanoseconds()) {
		t.Errorf("wait duration = %s, want at least 20ms", time.Duration(sample.WaitDuration))
	}
	if sample.Duration < sample.WriteDuration+sample.WaitDuration {
		t.Errorf("total duration %s is shorter than write plus wait %s", time.Duration(sample.Duration), time.Duration(sample.WriteDuration+sample.WaitDuration))
	}
}

func TestNestedRPCTimingDisabledWithoutPathOrForOtherLanguages(t *testing.T) {
	t.Setenv(nestedRPCTimingEnv, "")
	if conn := New(Config{Lang: "cpp"}); conn.requestTiming != nil {
		t.Fatal("timing enabled without an environment path")
	}

	t.Setenv(nestedRPCTimingEnv, filepath.Join(t.TempDir(), "rust-trace.json"))
	if conn := New(Config{Lang: "rust"}); conn.requestTiming != nil {
		t.Fatal("timing enabled for a non-cpp language")
	}
}

func TestNestedRPCTimingRecordsCancellationAndTimeout(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)
	stdout, server := io.Pipe()
	writer := &requestTimingTestWriter{stdout: server, requests: make(chan jsonrpc.Message, 4)}
	conn := New(Config{Lang: "cpp", RequestTimeout: 40 * time.Millisecond})
	conn.Attach(nil, writer, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = server.Close()
	})

	const documentURI = "file:///workspace/slow.cpp"
	params := map[string]any{"textDocument": map[string]string{"uri": documentURI}}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := conn.SendRequest(ctx, "textDocument/completion", params)
		canceled <- err
	}()
	select {
	case <-writer.requests:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("canceled request was not written")
	}
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v, want context.Canceled", err)
	}

	if _, err := conn.SendRequest(context.Background(), "textDocument/documentSymbol", params); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed request error = %v, want timeout", err)
	}
	if err := conn.Close(); err == nil || !strings.Contains(err.Error(), "shutdown request") {
		t.Fatalf("Close error = %v, want explicit shutdown-timeout lifecycle error", err)
	}

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read timing trace: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode timing trace: %v", err)
	}
	assertRequestTimingSamplesHavePhaseFields(t, data, 2)
	if len(report.Aggregates) != 2 {
		t.Fatalf("aggregates = %+v, want completion and documentSymbol", report.Aggregates)
	}
	for _, aggregate := range report.Aggregates {
		if aggregate.URI != documentURI || aggregate.Count != 1 {
			t.Errorf("aggregate = %+v, want one request for %q", aggregate, documentURI)
		}
		switch aggregate.Method {
		case "textDocument/completion":
			if aggregate.Canceled != 1 {
				t.Errorf("completion aggregate = %+v, want cancellation", aggregate)
			}
		case "textDocument/documentSymbol":
			if aggregate.TimedOut != 1 {
				t.Errorf("documentSymbol aggregate = %+v, want timeout", aggregate)
			}
		default:
			t.Errorf("unexpected method aggregate %+v", aggregate)
		}
	}
}

func TestNestedRPCTimingCloseFlushIncludesBridgeErrors(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)
	stdout, server := io.Pipe()
	writer := &requestTimingTestWriter{stdout: server, requests: make(chan jsonrpc.Message, 4)}
	conn := New(Config{Lang: "cpp", RequestTimeout: time.Second})
	conn.Attach(nil, writer, stdout)

	const documentURI = "file:///workspace/close.cpp"
	params := map[string]any{"textDocument": map[string]string{"uri": documentURI}}
	requestDone := make(chan error, 1)
	go func() {
		_, err := conn.SendRequest(context.Background(), "textDocument/completion", params)
		requestDone <- err
	}()
	select {
	case <-writer.requests:
	case <-time.After(time.Second):
		t.Fatal("in-flight request was not written")
	}
	if err := conn.Close(); err == nil || !strings.Contains(err.Error(), "shutdown request") {
		t.Fatalf("Close error = %v, want explicit shutdown-timeout lifecycle error", err)
	}
	if err := <-requestDone; err == nil {
		t.Fatal("closing connection unexpectedly succeeded")
	}

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read timing trace: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode timing trace: %v", err)
	}
	assertRequestTimingSamplesHavePhaseFields(t, data, 1)
	if len(report.Aggregates) != 1 || report.Aggregates[0].ErrorResponses != 1 {
		t.Fatalf("close-time bridge failure was not preserved distinctly: %+v", report.Aggregates)
	}
	if len(report.Samples) != 1 || report.Samples[0].Outcome != string(requestTimingErrorResponse) {
		t.Fatalf("close-time sample = %+v, want error_response", report.Samples)
	}
}

func TestNestedRPCTimingReportsCAndCppLanguages(t *testing.T) {
	for _, language := range []string{"c", "cpp"} {
		t.Run(language, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
			t.Setenv(nestedRPCTimingEnv, tracePath)
			recorder := newRequestTimingRecorder(language)
			if recorder == nil {
				t.Fatalf("%s timing recorder was not enabled", language)
			}
			recorder.record("textDocument/completion", "file:///workspace/main."+language, time.Millisecond, 400*time.Microsecond, 600*time.Microsecond, json.RawMessage("13"), requestTimingResponse)
			if err := recorder.flush(); err != nil {
				t.Fatalf("flush %s trace: %v", language, err)
			}
			data, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatalf("read %s trace: %v", language, err)
			}
			var report requestTimingReport
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatalf("decode %s trace: %v", language, err)
			}
			if report.Language != language {
				t.Errorf("trace language = %q, want %q", report.Language, language)
			}
			if len(report.Samples) != 1 || string(report.Samples[0].ParentRequestID) != "13" {
				t.Errorf("%s trace samples = %+v, want parent ID 13", language, report.Samples)
			}
			if report.SampleLimit != maxRequestTimingSamples || report.SamplesTruncated {
				t.Errorf("trace sample bounds = limit %d truncated %t", report.SampleLimit, report.SamplesTruncated)
			}
		})
	}
}

func TestNestedRPCTimingPreservesParentIDShapesAndLegacyTrace(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)
	recorder := newRequestTimingRecorder("cpp")
	const documentURI = "file:///workspace/parent-id.cpp"
	for _, parentID := range []json.RawMessage{json.RawMessage("41"), json.RawMessage(`"syntax-42"`), nil} {
		recorder.record("textDocument/completion", documentURI, time.Millisecond, 400*time.Microsecond, 600*time.Microsecond, parentID, requestTimingResponse)
	}
	if err := recorder.flush(); err != nil {
		t.Fatalf("flush trace: %v", err)
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if len(report.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(report.Samples))
	}
	if got := string(report.Samples[0].ParentRequestID); got != "41" {
		t.Errorf("numeric parent ID = %s, want 41", got)
	}
	if got := string(report.Samples[1].ParentRequestID); got != `"syntax-42"` {
		t.Errorf("string parent ID = %s, want %q", got, `"syntax-42"`)
	}
	if len(report.Samples[2].ParentRequestID) != 0 {
		t.Errorf("legacy sample parent ID = %s, want omitted", report.Samples[2].ParentRequestID)
	}
	var raw struct {
		Samples []map[string]json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw samples: %v", err)
	}
	if _, ok := raw.Samples[2]["parent_request_id"]; ok {
		t.Errorf("sample without parent ID unexpectedly emitted the field")
	}

	const oldTrace = `{"version":1,"language":"cpp","aggregates":[],"samples":[{"method":"textDocument/completion","uri":"file:///workspace/old.cpp","duration_ns":23,"outcome":"response"}]}`
	var oldReport requestTimingReport
	if err := json.Unmarshal([]byte(oldTrace), &oldReport); err != nil {
		t.Fatalf("decode legacy trace without phase or parent ID fields: %v", err)
	}
	if len(oldReport.Samples) != 1 || len(oldReport.Samples[0].ParentRequestID) != 0 {
		t.Fatalf("legacy sample decode = %+v", oldReport.Samples)
	}
}

func TestNestedRPCTimingMarksSampleOverflow(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-overflow.json")
	t.Setenv(nestedRPCTimingEnv, tracePath)
	recorder := newRequestTimingRecorder("cpp")
	for i := 0; i <= maxRequestTimingSamples; i++ {
		recorder.record("textDocument/completion", "file:///workspace/overflow.cpp", time.Nanosecond, time.Nanosecond, 0, nil, requestTimingResponse)
	}
	if err := recorder.flush(); err != nil {
		t.Fatalf("flush overflow trace: %v", err)
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read overflow trace: %v", err)
	}
	var report requestTimingReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode overflow trace: %v", err)
	}
	if !report.SamplesTruncated {
		t.Fatal("overflow trace was not marked truncated")
	}
	if report.SampleLimit != maxRequestTimingSamples || len(report.Samples) != maxRequestTimingSamples {
		t.Fatalf("overflow sample bounds = limit %d samples %d", report.SampleLimit, len(report.Samples))
	}
}
