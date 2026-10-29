package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/plugin"
)

// server 插件车道的端到端测试设施（§O0-O5）。沿用 internal/plugin 的
// TestHelperProcess 分流模式（helper_test.go:21-27）：宿主以本测试二进制
// 自身作为插件 entrypoint 启动子进程，环境变量 OMNILSP_SERVER_PLUGIN_HELPER
// 使 TestMain 进入插件行为模式而非跑测试。零额外二进制、跨平台。
func TestMain(m *testing.M) {
	if os.Getenv("OMNILSP_SERVER_PLUGIN_HELPER") != "" {
		omnilspPluginHelperMain()
		return
	}
	os.Exit(m.Run())
}

// omnilspPluginHelperMain 按 GO_PLUGIN_MODE 脚本化插件的线级行为：
//   - die          : 立即退出（崩溃路径，宿主 OnExit 收到非 nil err）；
//   - diag-emit    : 请求 method=="emit/now" 时先发一条 diagnostics/emit
//     通知帧再回 pong（敌意姿态：无论宿主授予什么都发——default deny 的
//     守卫在宿主侧）；
//   - diag-emit-die: emit/now → 先发 diagnostics/emit 通知帧并 flush，再以
//     非零码退出（emit 后崩溃：已注入宿主的诊断不随进程死亡回滚）；
//   - hold-shutdown: plugin/shutdown 请求不应答（存活挂起——closePlugins 的
//     Shutdown 超时路径）；plugin/exit 通知退出；其余请求回 pong。
//
// 全部模式统一处理 plugin/shutdown（回 null）与 plugin/exit（退出），
// 与宿主 ClosePlugins 的 LSP 语义握手兼容；其余请求回 pong。
func omnilspPluginHelperMain() {
	mode := os.Getenv("GO_PLUGIN_MODE")
	if mode == "die" {
		os.Exit(7) // 非零退出：RecordCrash 语义只对异常退出成立
	}

	reader := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	emitOnce := false
	for {
		line, err := readHelperFrame(reader)
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Args   json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(line, &req)
		if req.ID == nil {
			if req.Method == "plugin/exit" {
				_ = w.Flush()
				return // 干净退出（exit 0）
			}
			continue
		}
		id := *req.ID
		switch {
		case req.Method == "plugin/shutdown":
			if mode == "hold-shutdown" {
				continue // 挂起应答：存活但永不回应 shutdown 握手
			}
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":null}`, id)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case req.Method == "emit/now" && !emitOnce && (mode == "diag-emit" || mode == "diag-emit-die"):
			emitOnce = true
			uri := os.Getenv("GO_PLUGIN_EMIT_URI")
			if uri == "" {
				uri = "file:///p/main.go"
			}
			message := os.Getenv("GO_PLUGIN_EMIT_MESSAGE")
			if message == "" {
				message = "boom"
			}
			note := fmt.Sprintf(`{"jsonrpc":"2.0","method":"diagnostics/emit","params":{"uri":%q,"diagnostics":[{"startLine":0,"startChar":0,"endLine":0,"endChar":3,"severity":1,"code":"E1","message":%q}]}}`, uri, message)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(note), note)
			if err := w.Flush(); err != nil {
				return
			}
			if mode == "diag-emit-die" {
				os.Exit(7) // emit 后崩溃：宿主已注入的诊断不得回滚
			}
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"pong":true}}`, id)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		default:
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"pong":true}}`, id)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func readHelperFrame(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			return []byte(line), nil
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
		var size int
		if _, err := fmt.Sscanf(trimmed, "Content-Length: %d", &size); err != nil {
			return nil, err
		}
		if _, err := r.Discard(2); err != nil {
			return nil, err
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// writePluginDir 在 root 下装配一个插件目录：plugin.json + 复制当前测试
// 二进制为 entrypoint（checksum 按复制后的实算）。mutate 在 checksum 计算
// 之前执行，用于信任链失败样本（篡改二进制、声明原始 checksum）。
func writePluginDir(t *testing.T, root, id string, caps []plugin.Capability, mutate func(binPath string)) (dir string, m plugin.Manifest) {
	t.Helper()
	dir = filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	bin := filepath.Join(dir, filepath.Base(os.Args[0]))
	if err := copyFile(os.Args[0], bin); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	sum := sha256.Sum256(raw)
	declared := "sha256:" + hex.EncodeToString(sum[:])
	if mutate != nil {
		mutate(bin)
	}
	m = plugin.Manifest{
		ID:           id,
		Version:      "0.1.0",
		APIVersion:   plugin.SupportedAPIVersion,
		Entrypoint:   filepath.Base(os.Args[0]),
		Languages:    []string{"go"},
		Capabilities: caps,
		Checksum:     declared,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	return dir, m
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
