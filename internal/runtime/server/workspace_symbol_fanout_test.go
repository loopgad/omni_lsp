package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

type workspaceCountingBackend struct {
	mockBackend
	calls int
	err   error
}

func (b *workspaceCountingBackend) WorkspaceSymbols(context.Context, languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	b.calls++
	return b.wsymResult, b.err
}

func TestWorkspaceSymbolsQueriesAllUniqueBackends(t *testing.T) {
	s := New(DefaultConfig())
	shared := languages.WorkspaceSymbol{Name: "Shared", URI: "file:///shared.py", Kind: languages.SymbolFunction}
	goBackend := &workspaceCountingBackend{mockBackend: mockBackend{langID: "go", wsymResult: []languages.WorkspaceSymbol{{Name: "Zulu", URI: "file:///main.go"}, shared}}}
	pythonBackend := &workspaceCountingBackend{mockBackend: mockBackend{langID: "python", wsymResult: []languages.WorkspaceSymbol{shared, {Name: "Alpha", URI: "file:///main.py"}}}}
	s.RegisterBackend("go", goBackend)
	s.RegisterBackend("python", pythonBackend)
	s.RegisterBackend("py", pythonBackend)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.dispatcher.Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		t.Fatalf("workspace symbol response: %+v", response)
	}
	var symbols []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 3 || symbols[0].Name != "Alpha" || symbols[1].Name != "Shared" || symbols[2].Name != "Zulu" {
		t.Fatalf("mixed workspace symbols: %+v", symbols)
	}
	if goBackend.calls != 1 || pythonBackend.calls != 1 {
		t.Fatalf("alias fanout duplicated RPCs: go=%d python=%d", goBackend.calls, pythonBackend.calls)
	}
	pythonBackend.err = errors.New("upstream failed")
	response = s.dispatcher.Dispatch(context.Background(), request)
	if response == nil || response.Error == nil {
		t.Fatal("a backend failure became an apparently complete workspace result")
	}
}

func TestPersistentWorkspaceSymbolResponseCarriesItsLeasedGeneration(t *testing.T) {
	s, _, _ := newSemanticQueryFixture(t, 1)
	var evidence []identity.Evidence
	s.SetSemanticResponseObserver(func(_ jsonrpc.RequestID, values []identity.Evidence) { evidence = append(evidence, values...) })
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "workspace/symbol", json.RawMessage(`{"query":"Persistent"}`))
	response := s.dispatcher.Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		t.Fatalf("persistent workspace symbol response: %+v", response)
	}
	if len(evidence) != 1 || evidence[0].Kind != identity.EvidenceIndex || evidence[0].IndexGen != 1 || evidence[0].BuildContext == "" {
		t.Fatalf("workspace response lost generation provenance: %+v", evidence)
	}
}

func TestPersistentWorkspaceSymbolsValidateAndConvertSourcePositions(t *testing.T) {
	s, file, _ := newSemanticQueryFixture(t, 1)
	if err := os.WriteFile(file, []byte("package sample\n// 😀 PersistentTarget\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := fixtureSemanticProvider{definitionColumn: 6}
	s.RegisterSemanticIndexProvider("go", provider, provider)
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("reindex: %+v", response)
	}
	s.positionEncoding = "utf-8"
	symbols, used := s.persistentWorkspaceSymbols(context.Background(), "Persistent", s.currentRevision())
	if !used || len(symbols) != 1 || symbols[0].StartCol != 8 {
		t.Fatalf("UTF-8 workspace position: %+v used=%v", symbols, used)
	}
	if err := os.WriteFile(file, []byte("package sample\n// 😀 x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("reindex malformed fixture: %+v", response)
	}
	if symbols, used := s.persistentWorkspaceSymbols(context.Background(), "Persistent", s.currentRevision()); used {
		t.Fatalf("out-of-source position was served: %+v", symbols)
	}
}
