package nested

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	stderrors "errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/runtime/supervisor"
)

func TestNotifySourceChangesForwardsCanonicalOrderedWatchedFileEvents(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	dirty := []byte("value = 2\n")
	const openURI = "file:///workspace/open.py"
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 10, []SnapshotDocument{{
		URI: openURI, LangID: "python", Content: dirty,
	}})
	if err != nil {
		t.Fatalf("BeginWorkspaceSnapshot: %v", err)
	}
	_ = decodeRecordedNotification(t, <-recorder.frames) // unsaved buffer didOpen
	if err := finish(); err != nil {
		t.Fatalf("finish initial snapshot: %v", err)
	}
	before := conn.WorkspaceSnapshotGeneration()

	changes := []SourceChange{
		{URI: "FILE:///workspace/%64ep.py", Kind: 3}, // delete
		{URI: "file:///workspace/dep.py", Kind: 1},   // create, same identity
		{URI: "file:///workspace/other.py", Kind: 2}, // change
	}
	if err := conn.NotifySourceChanges(context.Background(), changes); err != nil {
		t.Fatalf("NotifySourceChanges: %v", err)
	}
	message := decodeRecordedNotification(t, <-recorder.frames)
	if message.Method != "workspace/didChangeWatchedFiles" {
		t.Fatalf("notification method = %q, want workspace/didChangeWatchedFiles", message.Method)
	}
	var raw struct {
		Changes []map[string]any `json:"changes"`
	}
	if err := json.Unmarshal(message.Params, &raw); err != nil {
		t.Fatalf("decode watched file params: %v", err)
	}
	wantURIs := []string{"file:///workspace/dep.py", "file:///workspace/dep.py", "file:///workspace/other.py"}
	wantKinds := []int{3, 1, 2}
	if len(raw.Changes) != len(wantURIs) {
		t.Fatalf("child changes = %d, want %d", len(raw.Changes), len(wantURIs))
	}
	for i, change := range raw.Changes {
		if change["uri"] != wantURIs[i] || change["type"] != float64(wantKinds[i]) {
			t.Fatalf("child change[%d] = %#v, want uri=%q type=%d", i, change, wantURIs[i], wantKinds[i])
		}
		if _, exists := change["URI"]; exists {
			t.Fatalf("child change[%d] uses non-standard URI field: %#v", i, change)
		}
		if _, exists := change["Kind"]; exists {
			t.Fatalf("child change[%d] uses non-standard Kind field: %#v", i, change)
		}
	}
	if got := conn.WorkspaceSnapshotGeneration(); got != before+1 {
		t.Fatalf("workspace generation = %d, want %d after source change", got, before+1)
	}
	conn.workspaceLease.mu.Lock()
	markerValid := conn.workspaceLease.marker.valid
	conn.workspaceLease.mu.Unlock()
	if markerValid {
		t.Fatal("workspace snapshot marker remained valid after external changes")
	}
	conn.mu.Lock()
	state := conn.documents[openURI]
	unchanged := state != nil && state.open && state.contentHash == hashDocumentContent(dirty)
	conn.mu.Unlock()
	if !unchanged {
		t.Fatal("external dependency changes modified or closed the dirty open buffer")
	}
}

func TestNotifySourceChangesWaitsForWorkspaceQueries(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 3, nil)
	if err != nil {
		t.Fatalf("BeginWorkspaceSnapshot: %v", err)
	}
	before := conn.WorkspaceSnapshotGeneration()
	done := make(chan error, 1)
	go func() {
		done <- conn.NotifySourceChanges(context.Background(), []SourceChange{{URI: "file:///workspace/dep.ts", Kind: 2}})
	}()
	waitForWaitingWorkspaceWriter(t, conn)
	select {
	case err := <-done:
		t.Fatalf("NotifySourceChanges returned while query lease was active: %v", err)
	case frame := <-recorder.frames:
		t.Fatalf("child was notified while query lease was active: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
	if err := finish(); err != nil {
		t.Fatalf("finish query lease: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("NotifySourceChanges after lease: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NotifySourceChanges did not proceed after query lease ended")
	}
	message := decodeRecordedNotification(t, <-recorder.frames)
	if message.Method != "workspace/didChangeWatchedFiles" {
		t.Fatalf("notification method = %q, want workspace/didChangeWatchedFiles", message.Method)
	}
	if got := conn.WorkspaceSnapshotGeneration(); got <= before {
		t.Fatalf("workspace generation = %d, want greater than %d", got, before)
	}
}

func TestNotifySourceChangesHonorsCanceledContextWhileWaiting(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 3, nil)
	if err != nil {
		t.Fatalf("BeginWorkspaceSnapshot: %v", err)
	}
	defer finish()
	before := conn.WorkspaceSnapshotGeneration()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- conn.NotifySourceChanges(ctx, []SourceChange{{URI: "file:///workspace/dep.ts", Kind: 2}})
	}()
	waitForWaitingWorkspaceWriter(t, conn)
	cancel()
	select {
	case err := <-done:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("NotifySourceChanges error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NotifySourceChanges did not honor context cancellation")
	}
	if got := conn.WorkspaceSnapshotGeneration(); got != before {
		t.Fatalf("workspace generation = %d after canceled notification, want %d", got, before)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("canceled notification reached child: %s", frame)
	default:
	}
}

func TestNotifySourceChangesHonorsCancellationWhileWaitingForWriteLock(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	before := conn.WorkspaceSnapshotGeneration()
	conn.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			conn.writeMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- conn.NotifySourceChanges(ctx, []SourceChange{{URI: "file:///workspace/dep.ts", Kind: 2}})
	}()
	waitForWaitingWrite(t, conn)
	cancel()
	select {
	case err := <-done:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("NotifySourceChanges error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NotifySourceChanges did not honor context cancellation while waiting for the write lock")
	}
	if got := conn.WorkspaceSnapshotGeneration(); got != before {
		t.Fatalf("workspace generation = %d after canceled write-lock wait, want %d", got, before)
	}
	conn.writeMu.Unlock()
	locked = false
	select {
	case frame := <-recorder.frames:
		t.Fatalf("canceled notification reached child after write lock was released: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestNotifySourceChangesCancellationFencesBlockedWriteEpoch(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, time.Second)
	childInput, childOutput := io.Pipe()
	entered := make(chan struct{})
	stdin := &enteredWriteCloser{WriteCloser: childOutput, entered: entered}
	conn.mu.Lock()
	conn.stdin = stdin
	conn.mu.Unlock()
	pending := conn.RegisterPending(101)
	before := conn.WorkspaceSnapshotGeneration()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- conn.NotifySourceChanges(ctx, []SourceChange{{URI: "file:///workspace/dep.ts", Kind: 2}})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("source notification did not enter the blocked pipe write")
	}
	cancel()
	select {
	case err := <-done:
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("NotifySourceChanges error = %v, want context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "write outcome uncertain") {
			t.Fatalf("NotifySourceChanges error = %v, want uncertain-write detail", err)
		}
		var uncertain *SourceChangeWriteUncertainError
		if !stderrors.As(err, &uncertain) || uncertain.Epoch == 0 {
			t.Fatalf("NotifySourceChanges error = %T %v, want epoch-fenced uncertainty", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("NotifySourceChanges did not return after cancellation closed the blocked pipe")
	}
	if got := conn.WorkspaceSnapshotGeneration(); got != before+1 {
		t.Fatalf("workspace generation = %d after uncertain write, want %d", got, before+1)
	}
	conn.mu.Lock()
	stdinDetached := conn.stdin == nil
	exitHandled := conn.exitHandled
	conn.mu.Unlock()
	if !stdinDetached || !exitHandled {
		t.Fatalf("blocked write epoch not fenced: stdin detached=%t exit handled=%t", stdinDetached, exitHandled)
	}
	select {
	case response := <-pending:
		if response == nil || response.Error == nil {
			t.Fatalf("pending request response = %#v, want process-terminated error", response)
		}
	case <-time.After(time.Second):
		t.Fatal("pending request was not failed when the blocked write epoch was fenced")
	}
	if _, err := childInput.Read(make([]byte, 1)); err == nil {
		t.Fatal("fenced child input pipe remained open")
	}
}

func TestCloseUnblocksBlockedSourceChangeWrite(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, time.Second)
	_, childOutput := io.Pipe()
	entered := make(chan struct{})
	conn.mu.Lock()
	conn.stdin = &enteredWriteCloser{WriteCloser: childOutput, entered: entered}
	conn.mu.Unlock()
	notifyDone := make(chan error, 1)
	go func() {
		notifyDone <- conn.NotifySourceChanges(context.Background(), []SourceChange{{URI: "file:///workspace/dep.ts", Kind: 2}})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source notification did not enter the blocked pipe write")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-notifyDone:
		var uncertain *SourceChangeWriteUncertainError
		if !stderrors.As(err, &uncertain) {
			t.Fatalf("NotifySourceChanges error = %T %v, want uncertain fenced write", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the blocked source notification writer")
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after releasing the blocked source writer")
	}
}

func TestRecoverSourceChangesWaitsForFreshReadyEpoch(t *testing.T) {
	spawns := &atomic.Int32{}
	conn := newTestConn(t, spawns)
	defer conn.Close()
	before := conn.SupervisorEpoch()
	if before == 0 {
		t.Fatal("initial supervisor epoch = 0, want a ready child")
	}
	if err := conn.RecoverSourceChanges(context.Background()); err != nil {
		t.Fatalf("RecoverSourceChanges: %v", err)
	}
	if got := conn.SupervisorEpoch(); got <= before {
		t.Fatalf("supervisor epoch = %d after recovery, want greater than %d", got, before)
	}
	if got := spawns.Load(); got != 2 {
		t.Fatalf("child starts = %d after recovery, want exactly 2", got)
	}
}

func TestRecoverSourceChangesTimeoutCanResumeWithoutSecondRetirement(t *testing.T) {
	sup := &supervisor.Config{
		MaxConsecutiveCrashes: 3,
		InitialBackoff:        50 * time.Millisecond,
		MaxBackoff:            100 * time.Millisecond,
		HungGrace:             time.Second,
	}
	spawns := &atomic.Int32{}
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: sup})
	conn.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("StartSupervised: %v", err)
	}
	defer conn.Close()
	before := conn.SupervisorEpoch()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	err := conn.RecoverSourceChanges(ctx)
	cancel()
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first RecoverSourceChanges error = %v, want context.DeadlineExceeded", err)
	}
	deadline := time.Now().Add(time.Second)
	for conn.SupervisorEpoch() == before {
		if time.Now().After(deadline) {
			t.Fatal("fresh supervisor epoch did not become ready after the first recovery timed out")
		}
		time.Sleep(time.Millisecond)
	}
	if err := conn.RecoverSourceChanges(context.Background()); err != nil {
		t.Fatalf("resumed RecoverSourceChanges: %v", err)
	}
	if got := conn.SupervisorEpoch(); got != before+1 {
		t.Fatalf("supervisor epoch = %d after resumed recovery, want %d", got, before+1)
	}
	if got := spawns.Load(); got != 2 {
		t.Fatalf("child starts = %d after resumed recovery, want exactly 2", got)
	}
}

func TestRecoverSourceChangesReportsQuarantine(t *testing.T) {
	sup := &supervisor.Config{
		MaxConsecutiveCrashes: 1,
		InitialBackoff:        time.Millisecond,
		MaxBackoff:            2 * time.Millisecond,
		HungGrace:             time.Second,
	}
	spawns := &atomic.Int32{}
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: sup})
	conn.cfg.Start = func(c *Conn) error { return fakeStart(c, spawns) }
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("StartSupervised: %v", err)
	}
	defer conn.Close()
	err := conn.RecoverSourceChanges(context.Background())
	if !stderrors.Is(err, supervisor.ErrQuarantined) {
		t.Fatalf("RecoverSourceChanges error = %v, want quarantine error", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("child starts = %d after quarantined recovery, want 1", got)
	}
}

func TestRecoverSourceChangesSucceedsBeforeAnyChildExists(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	if err := conn.RecoverSourceChanges(context.Background()); err != nil {
		t.Fatalf("cold RecoverSourceChanges: %v", err)
	}
}

type enteredWriteCloser struct {
	io.WriteCloser
	entered chan struct{}
	once    sync.Once
}

func (w *enteredWriteCloser) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.WriteCloser.Write(p)
}

func waitForWaitingWrite(t *testing.T, conn *Conn) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if conn.writeMu.waiters.Load() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("context-aware nested write did not wait for the write lock")
}

func TestNotifySourceChangesReturnsTransportError(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, time.Second)
	want := stderrors.New("synthetic write failure")
	conn.mu.Lock()
	conn.stdin = failingNotificationRecorder{err: want}
	conn.mu.Unlock()
	if err := conn.NotifySourceChanges(context.Background(), []SourceChange{{URI: "file:///workspace/dep.py", Kind: 2}}); !stderrors.Is(err, want) {
		t.Fatalf("NotifySourceChanges error = %v, want transport error %v", err, want)
	}
}

func TestNotifySourceChangesSkipsNeverStartedConnAndWorksAfterAttach(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	change := []SourceChange{{URI: "file:///workspace/dep.py", Kind: 2}}
	if err := conn.NotifySourceChanges(context.Background(), change); err != nil {
		t.Fatalf("never-started NotifySourceChanges: %v", err)
	}
	if got := conn.WorkspaceSnapshotGeneration(); got != 0 {
		t.Fatalf("never-started workspace generation = %d, want 0", got)
	}

	reader, writer := io.Pipe()
	recorder := newNotificationRecorder()
	conn.Attach(nil, recorder, reader)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = writer.Close()
	})
	if err := conn.NotifySourceChanges(context.Background(), change); err != nil {
		t.Fatalf("NotifySourceChanges after Attach: %v", err)
	}
	message := decodeRecordedNotification(t, <-recorder.frames)
	if message.Method != "workspace/didChangeWatchedFiles" {
		t.Fatalf("notification method = %q, want workspace/didChangeWatchedFiles", message.Method)
	}
}

func TestNotifySourceChangesReportsMissingStdinAfterStart(t *testing.T) {
	conn, _, _ := newDiagnosticsConn(t, time.Second)
	conn.mu.Lock()
	conn.stdin = nil
	conn.mu.Unlock()
	if err := conn.NotifySourceChanges(context.Background(), []SourceChange{{URI: "file:///workspace/dep.py", Kind: 2}}); err == nil {
		t.Fatal("NotifySourceChanges succeeded after a started child lost its stdin")
	}
}

func waitForWaitingWorkspaceWriter(t *testing.T, conn *Conn) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn.workspaceLease.mu.Lock()
		waiting := conn.workspaceLease.waitingWriters
		conn.workspaceLease.mu.Unlock()
		if waiting > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("workspace writer did not begin waiting")
}

func hashDocumentContent(content []byte) [32]byte {
	return sha256.Sum256(content)
}
