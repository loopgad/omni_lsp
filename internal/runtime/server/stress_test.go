package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"runtime"
)

// TestStressConcurrentRequests exercises many concurrent requests against the server
// to verify no deadlock, no goroutine leak, and valid terminal states.
// Corresponds to goal.md §S15 (stress testing).
func TestStressConcurrentRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := New(DefaultConfig())
	startGoroutines := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			params := `{"textDocument":{"uri":"file:///test.go","version":1,"languageId":"go"},"contentChanges":[{"range":null,"text":"package main\n"}]}`
			resp := srv.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(
				jsonrpc.RequestID{Num: int64(id)},
				"textDocument/didChange",
				json.RawMessage(params),
			))
			if resp == nil {
				t.Errorf("nil response for request %d", id)
			}
		}(i)
	}
	wg.Wait()

	afterGoroutines := runtime.NumGoroutine()
	growth := afterGoroutines - startGoroutines
	if growth > 50 {
		t.Errorf("goroutine growth %d exceeds tolerance", growth)
	}

	cancel()
	resp := srv.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 9999},
		"shutdown",
		nil,
	))
	if resp == nil {
		t.Error("server unresponsive after stress")
	}
}

// TestStressRapidCancel tests repeated cancel/start cycles.
// Corresponds to goal.md §S15 (repeated cancellation).
func TestStressRapidCancel(t *testing.T) {
	for round := 0; round < 50; round++ {
		ctx, cancel := context.WithCancel(context.Background())

		srv := New(DefaultConfig())
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				params := `{"textDocument":{"uri":"file:///x.go","version":1,"languageId":"go"},"contentChanges":[{"range":null,"text":"x"}]}`
				_ = srv.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(
					jsonrpc.RequestID{Num: int64(round*10 + i)},
					"textDocument/didChange",
					json.RawMessage(params),
				))
			}()
		}

		time.Sleep(5 * time.Millisecond)
		cancel()
		wg.Wait()
	}
}

// TestStressManyDocuments exercises many open/close cycles.
// Corresponds to goal.md §S15 (many open documents).
func TestStressManyDocuments(t *testing.T) {
	ctx := context.Background()
	srv := New(DefaultConfig())

	for doc := 0; doc < 200; doc++ {
		uri := "file:///test/doc_" + itoa(doc) + ".go"
		params := `{"textDocument":{"uri":"` + uri + `","version":1,"languageId":"go"},"contentChanges":[{"range":null,"text":"package test\n"}]}`
		resp := srv.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: int64(doc)},
			"textDocument/didOpen",
			json.RawMessage(params),
		))
		if resp == nil {
			t.Fatalf("nil response for open %d", doc)
		}
	}

	for doc := 0; doc < 200; doc++ {
		uri := "file:///test/doc_" + itoa(doc) + ".go"
		params := `{"textDocument":{"uri":"` + uri + `","version":1}}`
		_ = srv.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: int64(doc + 200)},
			"textDocument/didClose",
			json.RawMessage(params),
		))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
