package server

// §7/方案B 落地后的钉子：semanticTokensProvider 只在 initialize 时存在"真发
// token"的已注册后端时才声明。能力位是 SemanticTokensEmitter —— 刻意与
// languages.EncodedSemanticTokensProvider 同一方法集，即 golang 唯一真实现
// 的 encoding-aware token 路径；ccls/typescript/pyright/rust-analyzer 的
// Backend.SemanticTokens 仍是显式空桩（Q3），为桩声明能力会让符合 LSP 语义
// 的客户端关掉自己的高亮。golang 场景必须声明（含 Full 与 §I21 legend），
// ccls-only 与空注册场景必须不声明。任一端漂移本测试都要红。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// initializeCapabilities drives the real initialize handshake (no rootUri, so
// no index bootstrap is needed) and returns the advertised capabilities.
func initializeCapabilities(t *testing.T, langID string, be languages.Backend) ServerCapabilities {
	t.Helper()
	s := New(DefaultConfig())
	if be != nil {
		s.RegisterBackend(langID, be)
	}
	resp, err := s.handleInitialize(context.Background(), &jsonrpc.Message{Method: "initialize"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var result InitializeResult
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	return result.Capabilities
}

func TestQ3_SemanticTokensDeclaredOnlyForRealEmitters(t *testing.T) {
	t.Run("golang backend declares the capability", func(t *testing.T) {
		be := golang.New(t.TempDir())
		if _, ok := any(be).(SemanticTokensEmitter); !ok {
			t.Fatal("golang backend stopped implementing SemanticTokensEmitter — the capability would silently vanish for Go clients")
		}
		caps := initializeCapabilities(t, "go", be)
		if caps.SemanticTokensProvider == nil {
			t.Fatal("semanticTokensProvider not declared although a real emitter (golang) is registered")
		}
		if !caps.SemanticTokensProvider.Full {
			t.Error("semanticTokensProvider lost the full-document flag")
		}
		if got := len(caps.SemanticTokensProvider.Legend.TokenTypes); got != len(languages.SemanticTokenTypes) {
			t.Errorf("legend carries %d token types, want the canonical %d", got, len(languages.SemanticTokenTypes))
		}
	})

	t.Run("ccls-only server does not declare the capability", func(t *testing.T) {
		be := &ccls.Backend{}
		if _, ok := any(be).(SemanticTokensEmitter); ok {
			t.Fatal("ccls backend started implementing SemanticTokensEmitter — update this pin together with its stub")
		}
		caps := initializeCapabilities(t, "cpp", be)
		if caps.SemanticTokensProvider != nil {
			t.Fatalf("semanticTokensProvider declared for a stub-only backend: %+v", caps.SemanticTokensProvider)
		}
		// 桩端仍被服务：不声明时客户端保留自己的高亮；显式请求仍得到后端
		// 自己的（空）答案，而不是 method-not-found。
		tokens, err := be.SemanticTokens(context.Background(), "file:///w/main.cpp", []byte("int main() {}\n"))
		if err != nil {
			t.Fatalf("ccls SemanticTokens error: %v", err)
		}
		if len(tokens) != 0 {
			t.Fatalf("ccls SemanticTokens = %d tokens, want the explicit empty stub list", len(tokens))
		}
	})

	t.Run("no backend declares nothing", func(t *testing.T) {
		caps := initializeCapabilities(t, "", nil)
		if caps.SemanticTokensProvider != nil {
			t.Fatal("semanticTokensProvider declared with no backend registered")
		}
	})
}
