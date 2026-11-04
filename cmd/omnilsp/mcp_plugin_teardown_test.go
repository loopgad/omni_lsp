package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/plugin"
)

// 第 18 轮治理开口 2 守卫：--transport mcp 分支不经过 server.Run——Run 的
// defer 链（ClosePlugins 等）在该分支上不会执行，MCP 会话结束后插件子进程
// 无优雅 shutdown（shutdown→exit 握手永不发生），进程与 stdio 管道泄漏为
// 孤儿。修复形态：MCP 分支在 Serve 返回（含错误路径，fatalErr 的 os.Exit
// 不跑 defer）后显式补 ClosePlugins。
//
// 守卫以真实子进程端到端构造：本测试二进制经 TestMain 的
// OMNILSP_CMD_PLUGIN_HELPER 分流充当插件（最小协议循环），在 plugin/exit
// 通知到达时落一个 marker 文件后干净退出——marker 出现即「优雅拆除到达
// 插件」的实证。

// mcpPluginHelperMain 是守卫子进程的最小插件协议循环：忽略 plugin/hello，
// plugin/shutdown 以 null 应答，plugin/exit 通知到达时写 marker 文件（路径
// 取 OMNILSP_CMD_PLUGIN_EXIT_MARKER）后干净退出（exit 0）。其余请求回 pong。
func mcpPluginHelperMain() {
	if marker := os.Getenv("OMNILSP_CMD_PLUGIN_START_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte(fmt.Sprintf("pid=%d", os.Getpid())), 0o644)
	}
	reader := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for {
		line, err := readMCPHelperFrame(reader)
		if err != nil {
			return // stdin EOF/管道断：静默退出（红阶段父进程退出时的自然收场）
		}
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &req)
		if req.ID == nil {
			if req.Method == "plugin/exit" {
				if marker := os.Getenv("OMNILSP_CMD_PLUGIN_EXIT_MARKER"); marker != "" {
					_ = os.WriteFile(marker, []byte(fmt.Sprintf("pid=%d", os.Getpid())), 0o644)
				}
				_ = w.Flush()
				return // 干净退出（exit 0）
			}
			continue // plugin/hello 等宿主通知：忽略
		}
		var msg string
		if req.Method == "plugin/shutdown" {
			msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":null}`, *req.ID)
		} else {
			msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"pong":true}}`, *req.ID)
		}
		fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// readMCPHelperFrame 与 internal/plugin 的 readHelperFrame 同构的最小分帧读。
func readMCPHelperFrame(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return []byte(line), nil
			}
			return nil, err
		}
		trimmed := len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r')
		_ = trimmed
		if len(line) <= 2 { // 头块结束的空行
			continue
		}
		if len(line) > 15 && (line[0] == 'C' || line[0] == 'c') { // Content-Length 头
			var size int
			if _, err := fmt.Sscanf(line, "Content-Length: %d", &size); err != nil {
				return nil, err
			}
			if _, err := r.Discard(2); err != nil { // \r\n
				return nil, err
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, err
			}
			return buf, nil
		}
	}
}

// writeMCPGuardPluginDir 在 root 下装配守卫插件目录（manifest + 测试二进制
// entrypoint，checksum 按实算）。
func writeMCPGuardPluginDir(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	bin := filepath.Join(dir, filepath.Base(os.Args[0]))
	in, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatalf("open test binary: %v", err)
	}
	defer in.Close()
	out, err := os.Create(bin)
	if err != nil {
		t.Fatalf("create entrypoint: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close entrypoint: %v", err)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	sum := sha256.Sum256(raw)
	m := plugin.Manifest{
		ID:           id,
		Version:      "0.1.0",
		APIVersion:   plugin.SupportedAPIVersion,
		Entrypoint:   filepath.Base(os.Args[0]),
		Languages:    []string{"go"},
		Capabilities: []plugin.Capability{plugin.CapDiagnosticsEmit},
		Checksum:     "sha256:" + hex.EncodeToString(sum[:]),
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	return dir
}

// TestMCPTransportShutsPluginsDownGracefully：MCP 会话（stdin 立即 EOF）结束
// 后，已装配启动的插件必须在宿主进程退出前收到 shutdown→exit 优雅拆除
// （marker 文件落盘）。红：MCP 分支不调 ClosePlugins，marker 永不出现。
func TestMCPTransportShutsPluginsDownGracefully(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	t.Setenv("OMNILSP_CMD_PLUGIN_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "exit-marker")
	t.Setenv("OMNILSP_CMD_PLUGIN_EXIT_MARKER", marker)
	startMarker := filepath.Join(t.TempDir(), "started")
	t.Setenv("OMNILSP_CMD_PLUGIN_START_MARKER", startMarker)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pluginBase := t.TempDir()
	writeMCPGuardPluginDir(t, pluginBase, "mcp-teardown")
	pluginRoot := pluginBase // --plugins 指向插件根（Discover 扫其直接子目录）

	cfg := config.Default()
	cfg.Transport = "mcp"
	cfg.IndexDir = filepath.Join(t.TempDir(), "index")
	cfg.Backends = []config.BackendConfig{
		{LanguageID: "go", Enabled: false},
		{LanguageID: "c", Enabled: false},
		{LanguageID: "cpp", Enabled: false},
		{LanguageID: "rust", Enabled: false},
		{LanguageID: "python", Enabled: false},
		{LanguageID: "typescript", Enabled: false},
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "omnilsp.json")
	if err := os.WriteFile(configPath, configJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		t.Fatal(err)
	}
	previousStdin, previousStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inRead, outWrite
	defer func() { os.Stdin, os.Stdout = previousStdin, previousStdout }()
	defer func() { _ = inRead.Close(); _ = outRead.Close() }()

	// Round-23 OPT-3：插件 launch 已后台化——立即 EOF 会让会话拆除赢得对
	// launch 的竞速，按已 pin 的关闭契约（ClosePlugins 置位后的起飞必须被
	// 丢弃）插件根本不会启动，本测试的前提（优雅拆除一个运行中的插件）就
	// 落空了。改为在插件真正进入协议循环（start marker 落盘）后再 EOF。
	go func() {
		startDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(startDeadline) {
			if _, err := os.Stat(startMarker); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = inWrite.Close()
	}()

	cmdServe([]string{"--config", configPath, "--transport", "mcp", "--workspace", root, "--plugins", pluginRoot})

	// 诊断半边（守卫先决）：子进程必须真的被装配启动过——start marker 在
	// 有界窗口内未落盘才断言装配未发生（红阶段 cmdServe 立即返回，子进程
	// 可能尚在 exec 启动中，立即 stat 会误报）。
	startDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(startDeadline) {
		if _, err := os.Stat(startMarker); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(startMarker); err != nil {
		t.Fatalf("插件子进程未进入协议循环（start marker 未落盘: %v）——装配/Launch 未发生，先修装配再测拆除", err)
	}

	// MCP 会话返回后，插件进程必须已被优雅拆除（shutdown→exit 握手完成、
	// 子进程写 marker 后干净退出）。宿主进程此刻仍存活——拆除断言的是
	// 「Serve 返回后、进程退出前」的时点，与 Run 路径的 defer 语义对齐。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("MCP 会话结束后 %v 内插件未收到优雅拆除（marker 未落盘）——ClosePlugins 未接入 MCP 退出路径", 15*time.Second)
}
