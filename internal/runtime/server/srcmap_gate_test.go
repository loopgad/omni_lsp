package server

// Integration test for goal.md §D12/§Y0: the rename S3 gate must fail closed
// on virtual documents carrying UnmappedGenerated regions, while plain host
// documents keep renaming normally (SEM-SAFE-001).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
	"github.com/omnilsp/omni/internal/workspace/virtual"
)

func TestSrcmapGate_RenameBlockedByUnmappedGenerated(t *testing.T) {
	const hostURI = "file:///gen/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}, renResult: languages.ValidatedEdit{
		Complete: true,
		Edits:    []languages.TextEdit{{URI: hostURI, StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 7, NewText: "renamed"}},
	}})
	s.vfs.Open(hostURI, "go", 1, []byte("package main\n"), vfs.SourceEditor)

	virtURI := hostURI + "#vdoc"
	err := s.VirtualRegistry().RegisterVirtual(virtURI, hostURI, []virtual.Segment{
		{HostStart: 0, HostEnd: 13, VirtStart: 0, VirtEnd: 13, Quality: virtual.QualityExact},
		{HostStart: 13, HostEnd: 13, VirtStart: 13, VirtEnd: 64, Quality: virtual.QualityUnmappedGenerated},
	})
	if err != nil {
		t.Fatalf("RegisterVirtual: %v", err)
	}

	renameResp := func(uri string) *jsonrpc.Message {
		params := fmt.Sprintf(`{"textDocument":{"uri":%q},"position":{"line":0,"character":0},"newName":"zz"}`, uri)
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/rename", json.RawMessage(params))
		return s.dispatcher.Dispatch(context.Background(), msg)
	}

	t.Run("virtual document with unmapped region is rejected", func(t *testing.T) {
		resp := renameResp(virtURI)
		if resp == nil || resp.Error == nil {
			t.Fatalf("expected rename on %s to be rejected, got %+v", virtURI, resp)
		}
		if !strings.Contains(resp.Error.Message, "generat") {
			t.Errorf("error message = %q, want generated-region semantics", resp.Error.Message)
		}
	})

	t.Run("plain host document unaffected", func(t *testing.T) {
		resp := renameResp(hostURI)
		if resp == nil || resp.Error != nil {
			t.Fatalf("host rename failed: %+v", resp)
		}
		var edit struct {
			Changes map[string][]struct {
				NewText string `json:"newText"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(resp.Result, &edit); err != nil {
			t.Fatalf("unmarshal workspace edit: %v", err)
		}
		if len(edit.Changes) != 1 {
			t.Errorf("changes len = %d, want 1", len(edit.Changes))
		}
	})
}
