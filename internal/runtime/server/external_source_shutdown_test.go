package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// sourceChangeLeaseBackend models a child query that still holds a workspace
// read lease. The watcher cannot forward a source change until that lease ends.
type sourceChangeLeaseBackend struct {
	mockBackend
	readLeaseReleased chan struct{}
	notifyStarted     chan struct{}
	notifyOnce        sync.Once
}

func (b *sourceChangeLeaseBackend) NotifySourceChanges(ctx context.Context, _ []languages.SourceChange) error {
	b.notifyOnce.Do(func() { close(b.notifyStarted) })
	select {
	case <-b.readLeaseReleased:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestExternalSourceChangeDoesNotHoldShutdownBehindChildLease(t *testing.T) {
	backend := &sourceChangeLeaseBackend{
		mockBackend:       mockBackend{langID: "typescript", exts: []string{".ts", ".js"}},
		readLeaseReleased: make(chan struct{}),
		notifyStarted:     make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseLease := func() { releaseOnce.Do(func() { close(backend.readLeaseReleased) }) }
	s := New(DefaultConfig())
	s.RegisterBackend("typescript", backend)
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()
	transport := attachTransport(s)

	callbackStarted := make(chan struct{})
	callbackDone := make(chan struct{})
	var callbackErr error
	go func() {
		// This is the workspace watcher callback's serialization boundary.
		func() {
			s.mutationMu.Lock()
			defer s.mutationMu.Unlock()
			close(callbackStarted)
			callbackErr = s.applyExternalSourceChanges(context.Background(), []languages.SourceChange{{
				URI: "file:///workspace/package.json", Kind: languages.SourceChangeChanged,
			}})
		}()
		close(callbackDone)
	}()
	shutdownDone := make(chan struct{})
	var shutdownStarted bool
	defer func() {
		releaseLease()
		select {
		case <-callbackDone:
		case <-time.After(3 * time.Second):
			t.Error("source-change callback did not finish during cleanup")
		}
		if shutdownStarted {
			select {
			case <-shutdownDone:
			case <-time.After(3 * time.Second):
				t.Error("shutdown goroutine did not finish during cleanup")
			}
		}
	}()
	<-callbackStarted

	// The old synchronous fanout reaches NotifySourceChanges and remains inside
	// mutationMu while the active child lease is held. A deferred-event path can
	// finish the watcher callback without making this child call.
	select {
	case <-backend.notifyStarted:
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("source-change callback neither forwarded nor queued the event")
	}

	shutdownStarted = true
	go func() {
		s.dispatchInline(jsonrpc.NewRequest(jsonrpc.RequestID{Num: 73}, "shutdown", nil))
		close(shutdownDone)
	}()

	// Eglot 30.2's request timeout is 1.5 s; leave a small scheduling margin.
	select {
	case <-shutdownDone:
	case <-time.After(1400 * time.Millisecond):
		releaseLease()
		select {
		case <-shutdownDone:
		case <-time.After(time.Second):
			t.Fatal("shutdown did not finish after releasing the child read lease")
		}
		t.Fatal("shutdown waited for watcher child I/O while holding the serialized writer")
	}

	select {
	case <-callbackDone:
		if callbackErr != nil {
			t.Fatalf("source-change callback: %v", callbackErr)
		}
	default:
		t.Fatal("shutdown completed while the source-change callback still held mutationMu")
	}
	response := responseForID(transport.responses(), 73)
	if response == nil || response.Error != nil {
		t.Fatalf("shutdown response = %+v, want success before releasing the child lease", response)
	}
	select {
	case <-backend.notifyStarted:
		t.Fatal("shutdown completed after forwarding a queued source change without a query")
	default:
	}
}
