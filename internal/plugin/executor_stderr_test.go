package plugin

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// 复证圈 5 sweep 发现 5 的守卫：插件子进程 stderr 不得直通宿主 stderr——
// 旧实现 cmd.Stderr = os.Stderr 让插件向操作日志流写入任意字节（伪造
// 'omnilsp: …' 前缀行、凭据形态文本），宿主对其无前缀隔离、无有界性，
// 操作者无法区分宿主行与插件行（日志可信边界在进程面上开放）。
//
// 捕获窗口说明：子进程的 stderr 经 exec 的拷贝 goroutine 异步抵达宿主侧
// 写点——重定向宿主 os.Stderr 到管道后由读者 goroutine 持续搬运，断言前
// 轮询到确定性终态信号（末行噪声 / 抑制标记）。

var (
	stderrCapMu sync.Mutex
	stderrCap   strings.Builder
)

func startStderrCapture(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				stderrCapMu.Lock()
				stderrCap.Write(buf[:n])
				stderrCapMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		os.Stderr = old
		_ = w.Close()
		_ = r.Close()
	})
}

func capturedStderr() string {
	stderrCapMu.Lock()
	defer stderrCapMu.Unlock()
	return stderrCap.String()
}

const pluginStderrPrefix = "omnilsp: plugin demo.plugin stderr: "

// TestPluginStderr_PrefixedAndIsolated 钉住前缀隔离：插件的每一行 stderr
// 都必须带 "omnilsp: plugin <id> stderr:" 前缀转发——伪造宿主前缀的裸行
// 不得以自己的形态出现在宿主日志流里。
func TestPluginStderr_PrefixedAndIsolated(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "stderr-noise")
	t.Setenv("GO_PLUGIN_STDERR_FLOOD", "3")
	startStderrCapture(t)

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow, nil)
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	if _, err := p.Call("probe/stderr", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	// 终态信号：末行噪声（0002）已被转发（或被预算抑制——本测预算不触发）。
	waitFor(t, func() bool {
		c := capturedStderr()
		return strings.Contains(c, "PLUGIN-NOISE-0002") ||
			strings.Contains(c, "转发预算")
	}, "插件 stderr 转发从未抵达宿主捕获窗口")

	c := capturedStderr()
	if !strings.Contains(c, pluginStderrPrefix+"omnilsp: forged host line from plugin") {
		t.Fatalf("伪造宿主前缀行未带插件前缀转发（直通形态）:\n%s", c)
	}
	if !strings.Contains(c, pluginStderrPrefix+"PLUGIN-NOISE-0000") {
		t.Fatalf("噪声行未带插件前缀转发:\n%s", c)
	}
	for _, line := range strings.Split(c, "\n") {
		if strings.HasPrefix(line, "omnilsp: forged host line") ||
			strings.HasPrefix(line, "PLUGIN-NOISE-") {
			t.Fatalf("发现裸的插件行（无插件前缀）:\n%s", line)
		}
	}
}

// TestPluginStderr_ForwardBudgetBounded 钉住有界性：转发总量受每进程字节
// 预算约束——超预算后发一次抑制标记，后续字节丢弃并计数，插件不得用
// stderr 洪泛宿主日志。
func TestPluginStderr_ForwardBudgetBounded(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "stderr-noise")
	t.Setenv("GO_PLUGIN_STDERR_FLOOD", "200") // ~200KiB > 转发预算
	startStderrCapture(t)

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow, nil)
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	if _, err := p.Call("probe/stderr", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	// 终态信号：抑制标记出现后，后续字节按构造丢弃，断言稳定。
	waitFor(t, func() bool {
		return strings.Contains(capturedStderr(), "转发预算")
	}, "stderr 超预算抑制标记从未出现（转发无界）")

	c := capturedStderr()
	if strings.Contains(c, "PLUGIN-NOISE-0199") {
		t.Fatalf("末行噪声越过转发预算抵达宿主（转发无界）:\n…%s", c[len(c)-400:])
	}
	if !strings.Contains(c, pluginStderrPrefix+"omnilsp: forged host line from plugin") {
		t.Fatalf("预算触发后前缀隔离仍必须生效:\n%s", c)
	}
}
