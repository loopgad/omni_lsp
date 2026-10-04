package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

type completionPhaseTraceTestBackend struct {
	mockBackend
	snapshotCalls   int
	completionCalls int
	sawTraceSpan    bool
	traceSpan       *completionPhaseTraceSpan
	completionErr   error
}

func (b *completionPhaseTraceTestBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	b.snapshotCalls++
	time.Sleep(time.Millisecond)
	return ctx, func() error { return nil }, nil
}

func (*completionPhaseTraceTestBackend) WorkspaceSnapshotGeneration() uint64 { return 1 }

func (b *completionPhaseTraceTestBackend) CompletionList(ctx context.Context, _ languages.CompletionRequest) (languages.CompletionList, error) {
	b.completionCalls++
	b.traceSpan = completionPhaseSpanFromContext(ctx)
	b.sawTraceSpan = b.traceSpan != nil
	time.Sleep(time.Millisecond)
	return languages.CompletionList{
		Items:        []languages.CompletionItem{{Label: "present"}},
		IsIncomplete: true,
	}, b.completionErr
}

func TestCompletionPhaseTraceDisabledByDefault(t *testing.T) {
	t.Setenv(completionPhaseTraceEnv, "")
	s := New(DefaultConfig())
	if s.completionPhaseTrace != nil {
		t.Fatal("completion phase trace should be disabled without its environment variable")
	}
}

func TestCompletionPhaseTraceCorrelatesPhasesAndWritesOnRunTeardown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completion-phase-trace.json")
	t.Setenv(completionPhaseTraceEnv, path)
	t.Setenv(completionPhaseTraceRunIDEnv, "trace-run-7")
	t.Setenv(completionPhaseTraceCandidateSHAEnv, "sha256:"+strings.Repeat("a", 64))

	s := New(DefaultConfig())
	backend := &completionPhaseTraceTestBackend{mockBackend: mockBackend{
		langID: "cpp",
		exts:   []string{".cpp"},
	}}
	s.RegisterBackend("cpp", backend)
	uri := "file:///workspace/main.cpp"
	s.vfs.Open(uri, "cpp", 1, []byte("int main() {}\n"), vfs.SourceEditor)
	s.publishSnapshot()

	msg := jsonrpc.NewRequest(
		jsonrpc.RequestID{Str: "cpp-request-7", IsStr: true},
		"textDocument/completion",
		json.RawMessage(`{"textDocument":{"uri":"file:///workspace/main.cpp"},"position":{"line":0,"character":3}}`),
	)
	response, err := s.handleCompletion(context.Background(), msg)
	if err != nil {
		t.Fatalf("handleCompletion: %v", err)
	}
	if !strings.Contains(string(response), `"label":"present"`) || !strings.Contains(string(response), `"isIncomplete":true`) {
		t.Fatalf("completion response lost list semantics: %s", response)
	}
	if backend.snapshotCalls != 1 || backend.completionCalls != 1 {
		t.Fatalf("backend calls: snapshot=%d completion=%d", backend.snapshotCalls, backend.completionCalls)
	}
	if !backend.sawTraceSpan {
		t.Fatal("completion context did not carry the phase trace span")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("trace must not be written per request, stat error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx, newFakeTransport()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace written by Run teardown: %v", err)
	}
	var report completionPhaseTraceReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode trace report: %v", err)
	}
	if report.Version != 2 || report.SampleLimit != maxCompletionPhaseSamples || len(report.Samples) != 1 {
		t.Fatalf("unexpected trace report metadata: %+v", report)
	}
	if report.RunID != "trace-run-7" || report.CandidateSHA256 != strings.Repeat("a", 64) || report.ProcessID != os.Getpid() || report.CreatedUTC.IsZero() {
		t.Fatalf("trace provenance = %+v", report)
	}
	sample := report.Samples[0]
	if string(sample.ParentRequestID) != `"cpp-request-7"` {
		t.Errorf("parent request ID = %s", sample.ParentRequestID)
	}
	if sample.LanguageID != "cpp" || sample.URI != uri {
		t.Errorf("sample identity = language %q, URI %q", sample.LanguageID, sample.URI)
	}
	if sample.BeginBackendWorkspaceSnapshotNS == 0 || sample.BackendCompletionListNS == 0 {
		t.Errorf("backend phase timers did not capture the test delays: %+v", sample)
	}
	if sample.SemanticDispatchNS == 0 || sample.DispatchOutcome != "success" || sample.SnapshotMode != "acquired" ||
		sample.ProviderKind != "completion_list" || sample.ProviderOutcome != "success" ||
		!sample.ProjectionAttempted || sample.ProjectionOutcome != "success" || sample.CompletionProjectionMarshalNS == 0 {
		t.Errorf("successful completion trace has incomplete phase evidence: %+v", sample)
	}
	for _, phaseField := range []string{"begin_backend_workspace_snapshot_ns", "backend_completion_list_ns", "completion_projection_marshal_ns"} {
		if !strings.Contains(string(data), `"`+phaseField+`"`) {
			t.Errorf("trace report omitted phase field %q: %s", phaseField, data)
		}
	}

	if err := s.completionPhaseTrace.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	dataAfterSecondClose, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace after second close: %v", err)
	}
	if string(dataAfterSecondClose) != string(data) {
		t.Fatal("a repeated close rewrote the trace report")
	}
}

func TestCompletionPhaseTraceProviderErrorDoesNotClaimProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completion-phase-error.json")
	t.Setenv(completionPhaseTraceEnv, path)
	t.Setenv(completionPhaseTraceRunIDEnv, "trace-error-run")
	t.Setenv(completionPhaseTraceCandidateSHAEnv, strings.Repeat("b", 64))

	s := New(DefaultConfig())
	backend := &completionPhaseTraceTestBackend{
		mockBackend:   mockBackend{langID: "cpp", exts: []string{".cpp"}},
		completionErr: errors.New("child completion failed"),
	}
	s.RegisterBackend("cpp", backend)
	uri := "file:///workspace/main.cpp"
	s.vfs.Open(uri, "cpp", 1, []byte("int main() {}\n"), vfs.SourceEditor)
	s.publishSnapshot()
	msg := jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 8},
		"textDocument/completion",
		json.RawMessage(`{"textDocument":{"uri":"file:///workspace/main.cpp"},"position":{"line":0,"character":3}}`),
	)
	if _, err := s.handleCompletion(context.Background(), msg); err == nil {
		t.Fatal("handleCompletion unexpectedly succeeded")
	}
	if err := s.completionPhaseTrace.close(); err != nil {
		t.Fatalf("close completion trace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var report completionPhaseTraceReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode trace report: %v", err)
	}
	if len(report.Samples) != 1 {
		t.Fatalf("trace sample count = %d, want 1", len(report.Samples))
	}
	sample := report.Samples[0]
	if sample.DispatchOutcome != "failed" || sample.SemanticDispatchNS == 0 || sample.SnapshotMode != "acquired" ||
		sample.ProviderKind != "completion_list" || sample.ProviderOutcome != "error" ||
		sample.ProjectionAttempted || sample.ProjectionOutcome != "" || sample.CompletionProjectionMarshalNS != 0 {
		t.Fatalf("provider error trace has incorrect phase evidence: %+v", sample)
	}
}

func TestScheduledCompletionPhaseTraceIncludesSchedulerAndResponseStages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduled-completion-trace.json")
	t.Setenv(completionPhaseTraceEnv, path)

	s := New(DefaultConfig())
	backend := &completionPhaseTraceTestBackend{mockBackend: mockBackend{
		langID: "cpp",
		exts:   []string{".cpp"},
	}}
	s.RegisterBackend("cpp", backend)
	uri := "file:///workspace/main.cpp"
	s.vfs.Open(uri, "cpp", 1, []byte("int main() {}\n"), vfs.SourceEditor)
	s.publishSnapshot()
	transport := newFakeTransport()
	s.mu.Lock()
	s.transport = transport
	s.state = StateRunning
	s.mu.Unlock()
	s.scheduler.Start(context.Background())
	defer s.scheduler.Shutdown()

	msg := jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 17},
		"textDocument/completion",
		json.RawMessage(`{"textDocument":{"uri":"file:///workspace/main.cpp"},"position":{"line":0,"character":3}}`),
	)
	s.scheduleMessage(context.Background(), msg)
	deadline := time.Now().Add(2 * time.Second)
	var response *jsonrpc.Message
	for time.Now().Before(deadline) {
		transport.mu.Lock()
		if len(transport.writes) > 0 {
			response = transport.writes[0]
		}
		transport.mu.Unlock()
		if response != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if response == nil || response.Error != nil || !strings.Contains(string(response.Result), `"label":"present"`) {
		t.Fatalf("scheduled completion response = %+v", response)
	}
	if err := s.completionPhaseTrace.close(); err != nil {
		t.Fatalf("close trace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var report completionPhaseTraceReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if len(report.Samples) != 1 {
		t.Fatalf("trace sample count = %d, want 1", len(report.Samples))
	}
	sample := report.Samples[0]
	if string(sample.ParentRequestID) != "17" || sample.LanguageID != "cpp" || sample.URI != uri {
		t.Fatalf("trace identity = %+v", sample)
	}
	if sample.SchedulePrepareNS == 0 || sample.SchedulerSubmitNS == 0 || sample.SchedulerQueueWaitNS == 0 ||
		sample.DispatcherCallNS == 0 || sample.SchedulerResultDeliveryNS == 0 || sample.ResponseWriteNS == 0 ||
		sample.ResponseOutcome != "success" || sample.ServerRoundTripNS == 0 {
		t.Fatalf("scheduled trace omitted outer request stages: %+v", sample)
	}
}

func TestCompletionPhaseTraceCapacityAndSafeClose(t *testing.T) {
	t.Run("sample capacity is bounded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bounded.json")
		recorder := newCompletionPhaseTraceRecorder(path, 1)
		for i := int64(1); i <= 2; i++ {
			span := recorder.begin("cpp", "file:///workspace/main.cpp", json.RawMessage("1"))
			span.sample.BackendCompletionListNS = uint64(i)
			span.finish()
		}
		if err := recorder.close(); err != nil {
			t.Fatalf("close recorder: %v", err)
		}
		var report completionPhaseTraceReport
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read report: %v", err)
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if len(report.Samples) != 1 || report.SampleLimit != 1 || !report.SamplesTruncated {
			t.Fatalf("capacity report = %+v", report)
		}
	})

	t.Run("late finish is dropped after close", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		recorder := newCompletionPhaseTraceRecorder(path, 2)
		span := recorder.begin("c", "file:///workspace/main.c", json.RawMessage("9"))
		if err := recorder.close(); err != nil {
			t.Fatalf("close recorder with pending sample: %v", err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read closed report: %v", err)
		}
		span.finish()
		if err := recorder.close(); err != nil {
			t.Fatalf("repeated close: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read report after late finish: %v", err)
		}
		if string(after) != string(before) {
			t.Fatal("late finish modified the closed report")
		}
		var report completionPhaseTraceReport
		if err := json.Unmarshal(after, &report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if report.DroppedPendingSamples != 1 || !report.SamplesTruncated || len(report.Samples) != 0 {
			t.Fatalf("pending close report = %+v", report)
		}
	})
}
