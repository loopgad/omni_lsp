package server

// §F9 deadline tests. goal.md §F9: "Protocol adapters MAY supply deadlines;
// otherwise server defaults apply" and a timeout must "stop work -> cleanup
// -> return typed timeout/unavailable result". The server used to never set
// scheduler.Request.Deadline, so the scheduler's watchDeadline mechanism
// never started and a stuck backend could pin a worker forever. These tests
// pin the server-side defaults (per §F8 cost class, configurable) and the
// end-to-end timeout path driven through Server.scheduleMessage.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestF9_DefaultConfigDerivesDeadlinesPerCostClass pins that the §F9 server
// defaults exist: every §F8 cost class carries a positive default deadline,
// and a class doing more work never gets a tighter deadline than the
// interactive lane.
func TestF9_DefaultConfigDerivesDeadlinesPerCostClass(t *testing.T) {
	cfg := DefaultConfig()
	for class := scheduler.CostTiny; class <= scheduler.CostVeryLarge; class++ {
		if cfg.RequestDeadlines[class] <= 0 {
			t.Fatalf("cost class %s has no default request deadline: §F9 server defaults are missing", class)
		}
	}
	tiny := cfg.RequestDeadlines[scheduler.CostTiny]
	for class := scheduler.CostSmall; class <= scheduler.CostVeryLarge; class++ {
		if cfg.RequestDeadlines[class] < tiny {
			t.Fatalf("cost class %s deadline %s is tighter than the interactive lane's %s", class, cfg.RequestDeadlines[class], tiny)
		}
	}
}

// TestF9_SchedulerDeadlineStopsSlowBackend pins the §F9 timeout path end to
// end: a scheduled request whose backend blocks past the configured deadline
// is stopped (its context cancelled) and answers the typed RequestCancelled
// response instead of pinning a worker forever.
func TestF9_SchedulerDeadlineStopsSlowBackend(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RequestDeadlines = map[scheduler.CostClass]time.Duration{
		scheduler.CostTiny: 150 * time.Millisecond,
	}
	s := New(cfg)
	ft := attachTransport(s)
	stop := startScheduler(s)
	defer stop()

	backend := &cancellationResponseBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		entered:     make(chan struct{}),
	}
	s.RegisterBackend("go", backend)
	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{"rootUri":"file:///ws"}`)))
	waitForState(t, s, StateInitializing)
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("initialized", nil))
	waitForState(t, s, StateRunning)
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(`{"textDocument":{"uri":"file:///ws/main.go","languageId":"go","version":1,"text":"package main\n"}}`)))
	waitFor(t, func() bool { return s.vfs.Get("file:///ws/main.go") != nil })

	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 2}, "textDocument/hover",
		json.RawMessage(`{"textDocument":{"uri":"file:///ws/main.go"},"position":{"line":0,"character":0}}`),
	))
	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hover backend did not start")
	}

	var terminal *jsonrpc.Message
	waitFor(t, func() bool {
		for _, msg := range ft.responses() {
			if msg.ID != nil && msg.ID.Num == 2 && msg.Error != nil {
				terminal = msg
				return true
			}
		}
		return false
	})
	if terminal.Error.Code != jsonrpc.RequestCancelled {
		t.Fatalf("timed-out request error code = %d (%s), want RequestCancelled", terminal.Error.Code, terminal.Error.Message)
	}
}

// TestF9_TimedOutNotificationStaysSilent pins the drainResult guard that the
// deadline mechanism makes reachable: a scheduler-level failure (deadline
// expiry while a notification is queued) must never write an error response —
// notifications have no wire ID, and drainResult must not fabricate one.
func TestF9_TimedOutNotificationStaysSilent(t *testing.T) {
	s := New(DefaultConfig())
	ft := attachTransport(s)
	stop := startScheduler(s)
	defer stop()

	req := &scheduler.Request{
		RequestID: "notification-without-id",
		CostClass: scheduler.CostMedium,
		// Already expired: watchDeadline fires at Submit, so the Result
		// carries the deadline error before any worker touches Execute.
		Deadline: time.Now().Add(-time.Second),
		Execute: func(context.Context, *snapshot.Snapshot) (any, error) {
			return nil, nil
		},
	}
	if result := s.scheduler.Submit(req); result != scheduler.Admitted {
		t.Fatalf("submit = %s, want Admitted", result)
	}
	s.drainResult(req, "") // returns once the already-past deadline fired
	if resps := ft.responses(); len(resps) != 0 {
		t.Fatalf("timed-out notification produced %d wire responses, want silence: %+v", len(resps), resps)
	}
}

// TestF9_LargeDefaultCoversColdWorkspaceExportEnvelope pins the §F9 Large
// default against workspace-scale work the server itself performs on a cold
// session: the first full-corpus textDocument/references request pays the
// one-shot snapshot-overlay facts export over the whole dirty overlay, which
// the x9-1 acceptance startup measured at minutes (the 3e22f85 base run
// completed soak/initial-queries only after a 13m12s startup; b56a775's
// 120s default answered RequestCancelled while the dispatcher was still
// exporting, failing soak/initial-queries on every subsequent run). The
// default must cover the same 300s per-request envelope the acceptance
// harness budgets for cold phases (OMNILSP_SOAK_REQUEST_BUDGET=300s, Y1-10)
// while staying below the VeryLarge reindex ceiling so the §F8 ladder keeps
// its ordering.
func TestF9_LargeDefaultCoversColdWorkspaceExportEnvelope(t *testing.T) {
	cfg := DefaultConfig()
	large := cfg.RequestDeadlines[scheduler.CostLarge]
	if large < 300*time.Second {
		t.Fatalf("CostLarge default deadline = %s, want at least the 300s cold-workspace export envelope", large)
	}
	if large >= cfg.RequestDeadlines[scheduler.CostVeryLarge] {
		t.Fatalf("CostLarge default %s must stay tighter than the VeryLarge ceiling %s (§F8 ladder ordering)", large, cfg.RequestDeadlines[scheduler.CostVeryLarge])
	}
}
