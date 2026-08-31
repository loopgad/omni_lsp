package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// helperMain 按 GO_PLUGIN_MODE 脚本化插件的线级行为：
//   - echo-hello: 首行吞掉 plugin/hello 通知帧，之后每个请求都以该帧的
//     params 作为 result 回复（用于验证握手 grants 过滤）；
//   - rpc-error : 第一个请求回复 error 帧；
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
			ID   int64           `json:"id"`
			Args json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(line, &req)

		switch mode {
		case "echo-hello":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}", req.ID, helloParams)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "rpc-error":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":42,\"message\":\"nope\"}}", req.ID)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "content-length":
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", req.ID, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		case "slow-roundtrip":
			if req.Args != nil {
				var payload map[string]any
				if err := json.Unmarshal(req.Args, &payload); err == nil && payload["uri"] == "file:///slow.go" {
					time.Sleep(300 * time.Millisecond)
				}
			}
			msg := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}", req.ID, req.Args)
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		default:
			fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}\n", req.ID, req.Args)
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
