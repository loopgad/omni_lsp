package plugin

import (
	"github.com/omnilsp/omni/internal/runtime/budget"
)

// 第 18 轮治理开口 5：插件→宿主通知缓冲区的 §F14 字节配额。
//
// F14 车道（DEF-BUDWIRE，internal/languages 后端缓冲半边）确立了接线形态：
// 预算来源是进程环境而非配置文件，未配置即整体关闭（X10 红线：默认行为
// 零变化），配置后拒绝 fail-closed 且可见计数、绝不静默。插件车道的缓冲区
// ——notifyCh（容量 pluginNotifyBuffer=64，单帧负载上限 maxPluginFrameSize
// =4MiB）——是同一故障面：64×4MiB ≈ 256MiB 的敌意/失控插件通知可驻留在
// 宿主内存里等 pump 消费。本文件把同一形态接到通知缓冲上。
//
// 预算面（与 F13/F14 两条车线的 env 约定同构，解析复用
// budget.ParsePositiveBytes 的 positiveIntFromEnv 语义——非法值 stderr 一条
// 且保持关闭，绝不猜测）：
//
//	OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES  单插件进程通知缓冲驻留字节上限
//
// 取舍登记：不使用 OMNILSP_BACKEND_QUOTA_<OWNER>_BYTES 的按 owner 面——
// 那是 languages 车道按后端身份（clangd/pyright/…）的共享 Manager 记账，
// 插件进程不是后端连接；插件车道取单一进程级旋钮（全部插件同一配额），
// 每个插件进程各自持有一个 Manager（预算按进程生效，与缓冲按进程驻留
// 一一对应）。
const pluginNotifyBudgetEnv = "OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES"

// notifyBudgetOwner 是每个插件进程 Manager 内的记账 owner 名。
const notifyBudgetOwner = "plugin-notify"

// notifyBudgetFromEnv 解析通知缓冲配额：未设置返回 nil（关闭——默认行为
// 零变化），非法值经 ParsePositiveBytes 宣告并保持关闭，正值返回按该值
// 构造的 Manager（全局硬限与 owner 配额同值：单 owner 单用途的预算面）。
func notifyBudgetFromEnv() *budget.Manager {
	quota, ok := budget.ParsePositiveBytes(pluginNotifyBudgetEnv)
	if !ok {
		return nil
	}
	m := budget.New(quota, quota)
	m.SetQuota(notifyBudgetOwner, quota)
	return m
}

// notifyEventBytes 是一帧通知在缓冲通道内的记账字节数：params 与 method 的
// 长度之和（frame 结构的支配项；Go 字符串头与通道元素的开销为常数，不记账
// ——记账目的是约束敌意/失控插件的驻留字节，保守下限即可）。
func notifyEventBytes(ev pluginNotifyEvent) int64 {
	return int64(len(ev.params) + len(ev.method))
}
