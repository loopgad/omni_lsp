package nested

// F14 backend-buffer budget guards for the nested stdio bridge
// (DEF-BUDWIRE, F14 half). The budget is latched from the environment by
// internal/languages; every test here pins its own environment and resets
// the latch so the default-off invariant stays observable per test.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// resetBudgetForTest pins budget variables for this test and resets the
// process-wide latch both before and after.
func resetBudgetForTest(t *testing.T, pairs map[string]string) {
	t.Helper()
	for name, value := range pairs {
		t.Setenv(name, value)
	}
	languages.ResetBackendBudgetForTest()
	t.Cleanup(languages.ResetBackendBudgetForTest)
}

// newBudgetTestConn builds a conn with one open document whose state is
// reachable directly, without spawning a real worker. readerEpoch stays at
// its initial value, so receiveDiagnostics(epoch) matches.
func newBudgetTestConn(t *testing.T, name string) (*Conn, *documentState, string) {
	t.Helper()
	conn := New(Config{Name: name, Lang: "test", WorkDir: t.TempDir(), RequestTimeout: time.Second})
	const uri = "file:///workspace/main.go"
	conn.mu.Lock()
	state := &documentState{
		langID:           "test",
		open:             true,
		epoch:            conn.readerEpoch.Load(),
		version:          1,
		diagnosticsReady: make(chan struct{}),
	}
	conn.storeDocumentLocked(uri, state)
	conn.mu.Unlock()
	t.Cleanup(func() { _ = conn.Close() })
	return conn, state, uri
}

func diagnosticsPayload(items int) json.RawMessage {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":5}},"message":"diagnostic message %d","source":"test"}`, i, i, i)
	}
	b.WriteString("]")
	return json.RawMessage(b.String())
}

func publishRaw(uri string, version int32, diagnostics json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(publishDiagnosticsParams{URI: uri, Version: &version, Diagnostics: diagnostics})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestDiagnosticsBudgetRefusalDropsPublicationAndCounts(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "16",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")

	// A publication whose wire staging exceeds the 16-byte budget is
	// refused: the previously stored diagnostics (none) stay, the refusal is
	// counted, nothing panics.
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))

	if len(state.diagnostics) != 0 {
		t.Fatalf("over-quota publication was staged anyway: %d diagnostics", len(state.diagnostics))
	}
	if state.diagnosticsReceived {
		t.Fatal("over-quota publication must not mark diagnostics received")
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got != 1 {
		t.Fatalf("refusals(fake-lsp) = %d, want 1 (fail-closed must be countable, not silent)", got)
	}
}

func TestDiagnosticsBudgetReplacementSwapsAccounting(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "1048576",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	first := diagnosticsPayload(3)
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, first))
	if len(state.diagnostics) == 0 {
		t.Fatal("in-budget publication must be stored")
	}
	if state.budgetReserved != int64(len(first)) {
		t.Fatalf("budgetReserved = %d, want the staged wire size %d", state.budgetReserved, len(first))
	}
	manager := languages.BackendBudget()
	if manager == nil {
		t.Fatal("budget configured but manager stayed nil")
	}
	total1, _ := manager.Usage()
	if total1 != int64(len(first)) {
		t.Fatalf("global usage after first publication = %d, want %d", total1, len(first))
	}

	second := diagnosticsPayload(8)
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, second))
	if len(state.diagnostics) != 8 {
		t.Fatalf("replacement publication stored %d diagnostics, want 8", len(state.diagnostics))
	}
	if state.budgetReserved != int64(len(second)) {
		t.Fatalf("budgetReserved after replacement = %d, want %d", state.budgetReserved, len(second))
	}
	total2, _ := manager.Usage()
	if total2 != int64(len(second)) {
		t.Fatalf("global usage after replacement = %d, want %d (old reservation must be released, not accumulated)", total2, len(second))
	}
}

func TestDiagnosticsBudgetCloseDocumentReleases(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "1048576",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))
	manager := languages.BackendBudget()
	if manager == nil {
		t.Fatal("budget configured but manager stayed nil")
	}
	if total, _ := manager.Usage(); total == 0 {
		t.Fatal("publication must hold a reservation before close")
	}

	// CloseDocument's trailing didClose write fails on this pipe-less conn
	// (never started); the §F14 release happens before that write, which is
	// what this test pins.
	_ = conn.CloseDocument(uri, 1)
	if state.budgetReserved != 0 {
		t.Fatalf("budgetReserved after close = %d, want 0", state.budgetReserved)
	}
	if total, _ := manager.Usage(); total != 0 {
		t.Fatalf("global usage after close = %d, want 0 (closing a document must release its staging bytes)", total)
	}
}

func TestDiagnosticsUnconfiguredBehaviorUnchanged(t *testing.T) {
	// Refusal counters are process-global and earlier tests may have
	// incremented this owner's; the unconfigured invariant is that THIS
	// publication adds none.
	refusalsBefore := languages.BackendBufferRefusals("fake-lsp")
	resetBudgetForTest(t, nil)

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	if languages.BackendBudget() != nil {
		t.Fatal("no budget variables configured; the latch must stay nil (X10)")
	}
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))
	if len(state.diagnostics) != 3 {
		t.Fatalf("unconfigured path stored %d diagnostics, want 3 (default behavior unchanged)", len(state.diagnostics))
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got != refusalsBefore {
		t.Fatalf("unconfigured path recorded %d new refusals, want 0", got-refusalsBefore)
	}
}

func TestReadLoopFrameCapRefusesOversizedFrames(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_FAKE_LSP_BYTES": "256",
	})

	var spawns atomic.Int32
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: fastSup()})
	// io.Pipe returns (reader, writer): the test writes frames into the
	// writer, the conn's readLoop consumes the reader.
	childReader, childWriter := io.Pipe()
	conn.cfg.Start = func(c *Conn) error {
		spawns.Add(1)
		// Every epoch attaches the same pipe: the first readLoop exits on
		// the refused frame, the replacement blocks on the still-open
		// writer until the test ends.
		c.Attach(nil, nopWriteCloser{}, childReader)
		c.MarkReady()
		return nil
	}
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// A 512-byte frame against a 256-byte owner quota: the pipe buffer
	// allocation is refused pre-allocation, the read loop ends, and the
	// refusal is counted before supervision takes over.
	frame := fmt.Sprintf("Content-Length: 512\r\n\r\n%s", strings.Repeat("x", 512))
	if _, err := io.WriteString(childWriter, frame); err != nil {
		t.Fatalf("write oversized frame: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if languages.BackendBufferRefusals("fake-lsp") >= 1 && spawns.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got < 1 {
		t.Fatalf("refusals(fake-lsp) = %d, want >= 1: an oversized frame must be a counted refusal", got)
	}
	if got := spawns.Load(); got < 2 {
		t.Fatalf("spawns = %d, want >= 2: refusing the frame must retire the reader (visible, not silent)", got)
	}
	_ = childWriter.Close()
	_ = conn.Close()
}

func TestReadLoopFrameCapUnconfiguredKeepsDefault(t *testing.T) {
	resetBudgetForTest(t, nil)

	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	codec := conn.newReadCodec()
	if codec.MaxMessageSize != jsonrpc.DefaultMaxMessageSize {
		t.Fatalf("unconfigured codec cap = %d, want the codec default %d (§C1 cap unchanged)", codec.MaxMessageSize, jsonrpc.DefaultMaxMessageSize)
	}
	// The configured side: a below-default owner quota tightens the cap.
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_FAKE_LSP_BYTES": "256",
	})
	conn2 := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	if got := conn2.newReadCodec().MaxMessageSize; got != 256 {
		t.Fatalf("configured codec cap = %d, want the owner quota 256", got)
	}
}
