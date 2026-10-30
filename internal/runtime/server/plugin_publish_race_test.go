package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 复证圈 2 审计发现 7（high）的守卫：插件诊断车道的发布读侧不得无锁迭代
// live inner map。旧实现去抖回调在 d.mu 内取出 pluginItems[uri] 的 map 引用
// 后解锁，publishPluginDiagnostics 无锁迭代它——与 injectPluginDiagnostics
// 对同一 inner map 的写入时间重叠即触发 Go 运行时不可恢复 fatal（concurrent
// map iteration and map write），整个 LSP 进程崩溃。修复形态：回调在 d.mu
// 临界区内把 inner map 拍成局部快照再解锁，发布只吃快照。
//
// 红的机制与确定性说明（诚实注记）：
//   1. Go race detector 对「读侧持锁取样、读侧无锁消费、写侧持锁写」的模式
//      结构性沉默——mutex 全序把读与后续写排成 happens-after；真实的红是
//      runtime 的 map 硬检查 fatal，只在时间重叠时触发。
//   2. 去抖有「追写」特性（fire 前的任何 inject 都把 fire 推迟 150ms），墙钟
//      猜测 fire 时刻不可靠。本测试用白盒确定信号：回调 fire 的第一个动作是
//      delete(pluginTimers, uri)（diagnostics.go 回调体）——写侧自旋轮询该
//      信号，fire 后 µs 级落写；20000 个插件键把 merge 迭代窗口拉宽到毫秒级，
//      落写与无锁迭代的时间重叠因此是确定性的。红阶段命中即整测试二进制
//      fatal crash；修复后迭代吃快照、与 live map 脱钩，稳定绿。
func TestPluginEmit_ConcurrentInjectDoesNotRacePublish(t *testing.T) {
	s := New(DefaultConfig())
	rt := attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)

	// 预置 20000 个插件键：merge（取键排序 + 逐键查读合并）窗口达毫秒级，
	// 且发布 payload 可控（每键 1 条）。
	const pluginCount = 20000
	one := []languages.Diagnostic{{
		StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 1,
		Severity: 1, Code: "E", Source: "plugin:base", Message: "boom",
	}}
	for i := 0; i < pluginCount; i++ {
		s.diag.injectPluginDiagnostics(fmt.Sprintf("p%05d", i), uri, one)
	}

	const cycles = 5
	for c := 0; c < cycles; c++ {
		rt.reset()
		// 布防：本次 inject 的去抖计时器是 pluginTimers[uri] 的当前值。
		s.diag.injectPluginDiagnostics("writer", uri, one)
		// 自旋等待 fire 的白盒信号：回调先 delete(pluginTimers, uri) 再取
		// 引用进入无锁 merge——信号出现即说明回调已拿到 live 引用。
		for {
			s.diag.mu.Lock()
			_, armed := s.diag.pluginTimers[uri]
			s.diag.mu.Unlock()
			if !armed {
				break
			}
		}
		// fire 后连续落写：range 迭代阶段仅数百 µs，单发落写易错过；
		// 连续 volley 保证有写落在 range/逐键查读窗口内（旧实现 fatal）。
		for i := 0; i < 300; i++ {
			s.diag.injectPluginDiagnostics("writer", uri, one)
		}
		time.Sleep(150 * time.Millisecond) // 留给本轮回调走完（merge+marshal ≈ 85ms）
	}
}
