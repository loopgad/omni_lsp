package server

// §C3 诚实标准的第二个钉子：四位可选能力（signatureHelp / formatting /
// inlayHint / declaration）与 semanticTokensProvider 同一套方案 B——只在
// initialize 时存在真正实现对应可选接口的已注册后端才声明。修复前这四位
// 无条件硬编码 true，而 Formatter/SignatureHelper/InlayHintProvider/
// DeclarationProvider 只有 golang 后端实现，其余后端一律 errNotSupported：
// declared-but-unserved is a lie。ccls-only 与空注册场景必须不声明，
// golang 场景必须四位全声明；任一端漂移本测试都要红。

import (
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
)

func TestC3_OptionalCapabilitiesDeclaredOnlyWhenServed(t *testing.T) {
	be := golang.New(t.TempDir())
	// 自检：golang 后端必须仍实现四个可选接口，否则声明位会静默消失。
	if _, ok := any(be).(languages.SignatureHelper); !ok {
		t.Fatal("golang backend stopped implementing SignatureHelper")
	}
	if _, ok := any(be).(languages.Formatter); !ok {
		t.Fatal("golang backend stopped implementing Formatter")
	}
	if _, ok := any(be).(languages.InlayHintProvider); !ok {
		t.Fatal("golang backend stopped implementing InlayHintProvider")
	}
	if _, ok := any(be).(languages.DeclarationProvider); !ok {
		t.Fatal("golang backend stopped implementing DeclarationProvider")
	}

	caps := initializeCapabilities(t, "go", be)
	if caps.SignatureHelpProvider == nil {
		t.Error("signatureHelpProvider not declared although golang serves it")
	}
	if !caps.DocumentFormattingProvider {
		t.Error("documentFormattingProvider not declared although golang serves it")
	}
	if !caps.InlayHintProvider {
		t.Error("inlayHintProvider not declared although golang serves it")
	}
	if !caps.DeclarationProvider {
		t.Error("declarationProvider not declared although golang serves it")
	}
}

func TestC3_StubBackendsDoNotDeclareOptionalCapabilities(t *testing.T) {
	be := &ccls.Backend{}
	for name, probe := range map[string]func(languages.Backend) bool{
		"SignatureHelper":     func(b languages.Backend) bool { _, ok := b.(languages.SignatureHelper); return ok },
		"Formatter":           func(b languages.Backend) bool { _, ok := b.(languages.Formatter); return ok },
		"InlayHintProvider":   func(b languages.Backend) bool { _, ok := b.(languages.InlayHintProvider); return ok },
		"DeclarationProvider": func(b languages.Backend) bool { _, ok := b.(languages.DeclarationProvider); return ok },
	} {
		if probe(be) {
			t.Fatalf("ccls backend started implementing %s — update this pin together with its stub", name)
		}
	}

	caps := initializeCapabilities(t, "cpp", be)
	if caps.SignatureHelpProvider != nil {
		t.Errorf("signatureHelpProvider declared for a stub-only backend: %+v", caps.SignatureHelpProvider)
	}
	if caps.DocumentFormattingProvider {
		t.Error("documentFormattingProvider declared for a stub-only backend")
	}
	if caps.InlayHintProvider {
		t.Error("inlayHintProvider declared for a stub-only backend")
	}
	if caps.DeclarationProvider {
		t.Error("declarationProvider declared for a stub-only backend")
	}
}

func TestC3_NoBackendNoOptionalCapabilityDeclarations(t *testing.T) {
	caps := initializeCapabilities(t, "", nil)
	if caps.SignatureHelpProvider != nil {
		t.Error("signatureHelpProvider declared with no backend registered")
	}
	if caps.DocumentFormattingProvider {
		t.Error("documentFormattingProvider declared with no backend registered")
	}
	if caps.InlayHintProvider {
		t.Error("inlayHintProvider declared with no backend registered")
	}
	if caps.DeclarationProvider {
		t.Error("declarationProvider declared with no backend registered")
	}
}
