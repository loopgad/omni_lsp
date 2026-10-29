package plugin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess 分流（Go 官方 exec 测试模式）：当宿主以本测试二进制
// 自身作为插件 entrypoint 启动子进程时，环境变量 OMNISP_PLUGIN_HELPER 使
// TestMain 进入插件行为模式而非跑测试。零额外二进制、跨平台。
func TestMain(m *testing.M) {
	if os.Getenv("OMNISP_PLUGIN_HELPER") != "" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// launchTestBinary 以测试二进制自身作为插件入口发起 Launch（§N14 目录来源
// 语义下的标准测试姿势）：目录来源取其所在目录，entrypoint 取裸文件名，
// checksum 按文件实算——Launch 启动前的 sha256 复核因此拿到真实匹配的清单。
func launchTestBinary(t *testing.T, m Manifest, g Grant, onExit func(error)) (*Process, error) {
	t.Helper()
	m.Entrypoint = filepath.Base(os.Args[0])
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		return nil, fmt.Errorf("读取测试二进制: %w", err)
	}
	sum := sha256.Sum256(raw)
	m.Checksum = "sha256:" + hex.EncodeToString(sum[:])
	return Launch(context.Background(), filepath.Dir(os.Args[0]), m, g, onExit)
}

// helperMain 按 GO_PLUGIN_MODE 脚本化插件的线级行为：
//   - echo-hello: 首行吞掉 plugin/hello 通知帧，之后每个请求都以该帧的
//     params 作为 result 回复（用于验证握手 grants 过滤）；
//   - rpc-error : 第一个请求回复 error 帧；
//   - reverse-roundtrip: 收到两次请求后按相反顺序回复（验证并发请求按 ID 匹配）；
//   - batch-frames: 收到两次请求后，把两帧响应拼接进一次写入再回复
//     （验证宿主能在单个数据块内切分连续帧），且每帧负载内嵌裸换行；
//   - extra-headers: 响应帧携带 Content-Type 附加头 + 小写 content-length，
//     负载内嵌裸换行（验证头块解析与负载内换行容错）；
//   - oversized-frame: 回复一个声明 99999999 字节的帧头后立即退出
//     （验证宿主单帧上限拒绝，而非按声明分配后 EOF）；
//   - garbage-then-reply: 先写一行不可解析垃圾再回复合法帧
//     （验证单帧损坏不中止连接、不殃及并发请求）；
//   - collect-notify: 记录 hello 之后所有无 id 的通知帧，收到
//     broadcast/audit 请求时把它们原样回传（验证 Notify 广播）；
//   - emit-notify    : 收到第一个请求时先向宿主发一条无 id 通知帧
//     （GO_PLUGIN_NOTIFY_METHOD，默认 diagnostics/emit），再回 pong
//     （验证宿主 readLoop 的 method 解析与订阅回调投递）；
//   - notify-flood   : 收到第一个请求时连续发 GO_PLUGIN_FLOOD 条通知帧，
//     之后才回 pong（验证有界投递：慢消费者下 readLoop 不死、丢弃计数）。
//   - graceful-shutdown: plugin/shutdown 请求以 null 应答并记录；
//     plugin/exit 通知只记录不退出（供宿主事后审计）；其余请求回显
//     （验证 shutdown 后响应仍可用、不合规 exit 零发送）；
//   - shutdown-exit  : 同 graceful-shutdown，但收到 plugin/exit 通知后
//     立即干净退出（exit 0）（验证 shutdown→exit 完整生命周期）；
//   - die       : 立即退出（覆盖宿主的 EOF/进程死亡路径）；
//   - roundtrip : 默认。回显 {"pong": <原样 params>}，id 与请求一致。
func helperMain() {
	mode := os.Getenv("GO_PLUGIN_MODE")
	switch mode {
	case "die":
		return
	}

	reader := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	var helloParams json.RawMessage
	sawHello := false
	var reverseBatch []struct {
		ID   int64
		Args json.RawMessage
	}
	var collected []json.RawMessage
	var sawShutdown, sawExit bool
	var emitted bool
	for {
		line, err := readHelperFrame(reader)
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}

		if !sawHello {
			var hf struct {
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(line, &hf)
			helloParams = hf.Params
			sawHello = true
			continue
		}

		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Args   json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(line, &req)
		if req.ID == nil {
			// 无 id 的帧是插件→宿主方向的帧在宿主→插件方向上的镜像：
			// 宿主发出的通知。只有 collect-notify 与 shutdown 两种模式关心。
			if mode == "collect-notify" {
				note := struct {
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}{Method: req.Method, Params: req.Args}
				if b, mErr := json.Marshal(note); mErr == nil {
					collected = append(collected, b)
				}
			}
			if req.Method == "plugin/exit" && (mode == "graceful-shutdown" || mode == "shutdown-exit") {
				sawExit = true
				if mode == "shutdown-exit" {
					if err := w.Flush(); err != nil {
						return
					}
					return // 干净退出（exit 0）：LSP 语义里 exit 即终止
				}
			}
			continue
		}
		id := *req.ID
		if mode == "reverse-roundtrip" {
			reverseBatch = append(reverseBatch, struct {
				ID   int64
				Args json.RawMessage
			}{ID: id, Args: req.Args})
			if len(reverseBatch) < 2 {
				continue
			}
			for i := len(reverseBatch) - 1; i >= 0; i-- {
				queued := reverseBatch[i]
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", queued.ID, queued.Args)
				if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg); err != nil {
					return
				}
			}
			reverseBatch = reverseBatch[:0]
			if err := w.Flush(); err != nil {
				return
			}
			continue
		}
		if mode == "batch-frames" {
			reverseBatch = append(reverseBatch, struct {
				ID   int64
				Args json.RawMessage
			}{ID: id, Args: req.Args})
			if len(reverseBatch) < 2 {
				continue
			}
			// 两帧拼接进一次写入；负载成员之间嵌裸换行——若宿主按行
			// 切帧（骨架旧行为）而非按 Content-Length 切帧，必解析失败。
			var both strings.Builder
			for _, queued := range reverseBatch {
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\n\"id\":%d,\"result\":{\"pong\":%s}}", queued.ID, queued.Args)
				fmt.Fprintf(&both, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			}
			if _, err := w.WriteString(both.String()); err != nil {
				return
			}
			reverseBatch = reverseBatch[:0]
			if err := w.Flush(); err != nil {
				return
			}
			continue
		}

		switch mode {
		case "echo-hello":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}", id, helloParams)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "emit-notify":
			if !emitted {
				emitted = true
				method := os.Getenv("GO_PLUGIN_NOTIFY_METHOD")
				if method == "" {
					method = "diagnostics/emit"
				}
				params := os.Getenv("GO_PLUGIN_NOTIFY_PARAMS")
				if params == "" {
					params = `{"uri":"file:///p/main.go"}`
				}
				note := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(note), note)
			}
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "notify-flood":
			if !emitted {
				emitted = true
				n, _ := strconv.Atoi(os.Getenv("GO_PLUGIN_FLOOD"))
				if n < 1 {
					n = 1
				}
				for i := 0; i < n; i++ {
					note := fmt.Sprintf(`{"jsonrpc":"2.0","method":"diagnostics/emit","params":{"seq":%d}}`, i)
					fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(note), note)
				}
			}
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "rpc-error":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":42,\"message\":\"nope\"}}", id)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "content-length":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "extra-headers":
			// 附加头 + 小写头名 + 负载内嵌裸换行：三者都必须被宿主容忍。
			msg := fmt.Sprintf("{\"jsonrpc\": \"2.0\",\n\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-length: %d\r\n\r\n%s", len(msg), msg)
		case "oversized-frame":
			// 声明远超单帧上限的长度后立即退出：宿主必须立刻以「超出
			// 上限」报错，而不是按声明值分配内存后等到 EOF。
			fmt.Fprint(w, "Content-Length: 99999999\r\n\r\n")
			if err := w.Flush(); err != nil {
				return
			}
			return
		case "garbage-then-reply":
			fmt.Fprint(w, "not-json-garbage\n")
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "slow-roundtrip":
			if req.Args != nil {
				var payload map[string]any
				if err := json.Unmarshal(req.Args, &payload); err == nil && payload["uri"] == "file:///slow.go" {
					time.Sleep(300 * time.Millisecond)
				}
			}
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "collect-notify":
			if req.Method == "broadcast/audit" {
				payload, _ := json.Marshal(collected)
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"notifications\":%s}}", id, payload)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			} else {
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			}
		case "graceful-shutdown", "shutdown-exit":
			switch req.Method {
			case "plugin/shutdown":
				sawShutdown = true
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":null}", id)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			case "audit/lifecycle":
				st, _ := json.Marshal(map[string]bool{"sawShutdown": sawShutdown, "sawExit": sawExit})
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}", id, st)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			default:
				msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", id, req.Args)
				fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
			}
		default:
			fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}\n", id, req.Args)
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
