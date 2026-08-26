package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
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
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	var helloParams json.RawMessage
	sawHello := false
	for sc.Scan() {
		line := sc.Bytes()
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
			fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n", req.ID, helloParams)
		case "rpc-error":
			fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":42,\"message\":\"nope\"}}\n", req.ID)
		default:
			fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"pong\":%s}}\n", req.ID, req.Args)
		}
		w.Flush()
	}
}
