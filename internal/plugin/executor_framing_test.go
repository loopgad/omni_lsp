package plugin

import (
	"bufio"
	"strings"
	"testing"
)

// TestReadFrame_ContentLengthFirstHeaderOrder 钉住 LSP 常见头序（审计批 1，
// O2 安全面）：Content-Length 可以出现在头块最前，随后跟 Content-Type 等
// 附加头——帧文档明言「附加头容忍、其余头按规范忽略」。旧实现在见到
// Content-Length 后把任何非空行判为 fatal，击穿这一头序（探针 A：
// 「Content-Length: 2\r\nContent-Type: …\r\n\r\n{}」被拒）。
func TestReadFrame_ContentLengthFirstHeaderOrder(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Length: 2\r\n" +
		"Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n{}"))
	payload, err := readFrame(r)
	if err != nil {
		t.Fatalf("Content-Length-first header order must parse: %v", err)
	}
	if string(payload) != "{}" {
		t.Fatalf("payload = %q, want {}", payload)
	}
}

// TestReadFrame_HeaderLineOverLimitIsFatal 钉住头行硬上限（审计批 1）：头行
// 原先经 ReadString('\n') 无界累积——对无换行流实测 64MiB 输入累积 64MiB 堆
// 内存，4MiB 单帧上限只约束 Content-Length 声明值不约束头块扫描本身，宿主可
// 被已运行/损坏插件 OOM。超限必须返回致命错误，与 EOF 同走 readLoop 的进程
// 遏制路径。
func TestReadFrame_HeaderLineOverLimitIsFatal(t *testing.T) {
	// 头形态的 1MiB 垃圾行：旧实现把它当附加头忽略后照常解析出负载，
	// 新实现必须在上限处拒绝，且错误指明头行上限。
	wire := "X-Junk: " + strings.Repeat("a", 1<<20) + "\r\nContent-Length: 2\r\n\r\n{}"
	r := bufio.NewReader(strings.NewReader(wire))
	_, err := readFrame(r)
	if err == nil {
		t.Fatal("header line beyond the hard cap must be rejected, not scanned unboundedly")
	}
	if !strings.Contains(err.Error(), "头行超出") {
		t.Fatalf("err = %v, want the header-line cap rejection", err)
	}
}

// TestReadFrame_SecondContentLengthIsFatal 钉住重复 Content-Length 的拒绝：
// 头块只允许一个 Content-Length，出现第二个时帧边界已不可信，必须 fatal。
// 旧实现同样拒绝（任何非空行即 fatal），此钉保证重写后该拒绝仅对此情形保留。
func TestReadFrame_SecondContentLengthIsFatal(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Length: 2\r\nContent-Length: 3\r\n\r\n{}"))
	if _, err := readFrame(r); err == nil {
		t.Fatal("duplicate Content-Length must be rejected")
	}
}

// TestReadFrame_NonHeaderLineAfterContentLengthIsFatal 钉住头块中非头形态行
// 的拒绝：Content-Length 之后出现裸 JSON 行仍是帧结构损坏，必须 fatal——
// 重写放开的是「附加头」，不是任意行。
func TestReadFrame_NonHeaderLineAfterContentLengthIsFatal(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Length: 2\r\nnot-a-header\r\n\r\n{}"))
	if _, err := readFrame(r); err == nil {
		t.Fatal("non-header line after Content-Length must be rejected")
	}
}

// TestReadFrame_StrayBlankLinesStillTolerated 钉住既有兼容行为：没有
// Content-Length 的帧间杂散空行继续被跳过，裸行负载（骨架路径）照常返回。
func TestReadFrame_StrayBlankLinesStillTolerated(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\r\n\r\n{}\n"))
	payload, err := readFrame(r)
	if err != nil {
		t.Fatalf("stray blank lines before a bare payload line must be tolerated: %v", err)
	}
	if string(payload) != "{}" {
		t.Fatalf("payload = %q, want {}", payload)
	}
}

// TestReadFrame_ContentTypeFirstStillParses 钉住既有头序（helper_test.go 的
// extra-headers 全进程钉住之外的直接单元钉）：Content-Type 在前、小写
// content-length 在后照常解析（探针 B）。
func TestReadFrame_ContentTypeFirstStillParses(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		"content-length: 5\r\n\r\nhello"))
	payload, err := readFrame(r)
	if err != nil {
		t.Fatalf("Content-Type-first header order must keep parsing: %v", err)
	}
	if string(payload) != "hello" {
		t.Fatalf("payload = %q, want hello", payload)
	}
}
