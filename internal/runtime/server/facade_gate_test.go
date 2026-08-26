package server

import (
	"context"
	"strings"
	"testing"
)

// TestARCH_CallMethodMutatingGated 证明 CallMethod 门面在非 Ready 状态拒绝
// 四个 mutating 方法（C2 门语义一致），VFS 不会被绕过生命周期门改写；
// Ready (Running) 状态下同一调用正常放行。
func TestARCH_CallMethodMutatingGated(t *testing.T) {
	s := New(DefaultConfig())
	ctx := context.Background()
	uri := "file:///gated/main.go"
	openParams := map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": "go", "version": 1, "text": "package main\n",
		},
	}

	for _, m := range []string{
		"textDocument/didOpen", "textDocument/didChange",
		"textDocument/didSave", "textDocument/didClose",
	} {
		_, err := s.CallMethod(ctx, m, openParams)
		if err == nil || !strings.Contains(err.Error(), "not allowed in state") {
			t.Errorf("%s in state %s: want lifecycle gate rejection, got %v", m, s.State(), err)
		}
	}

	if src := s.vfs.Content(uri); len(src) != 0 {
		t.Errorf("VFS mutated through gated facade: %q", src)
	}
	if f := s.vfs.Get(uri); f != nil {
		t.Error("document present in VFS after gated didOpen")
	}

	// Ready 状态下放行：直接置为 Running（等价于 initialize 握手完成），
	// didOpen 必须成功且文档进入 VFS。
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	if _, err := s.CallMethod(ctx, "textDocument/didOpen", openParams); err != nil {
		t.Fatalf("didOpen via facade in Running state: %v", err)
	}
	f := s.vfs.Get(uri)
	if f == nil {
		t.Fatal("didOpen in Running state did not reach VFS")
	}
	if f.LanguageID != "go" {
		t.Errorf("LanguageID = %q, want go", f.LanguageID)
	}
}
