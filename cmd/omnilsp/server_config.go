package main

import (
	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/runtime/server"
	"time"
)

// serverConfigFrom keeps live serving and replay bound to the same configured
// scheduler and persistent-index location.
//
// defaultWatchInterval is the §D14 workspace poller cadence: the same 2s
// magnitude the derived shutdown/external-sync budgets use
// (server.defaultDrainWindow), kept as a named constant here rather than a
// bare literal at the assignment.
const defaultWatchInterval = 2 * time.Second

func serverConfigFrom(cfg config.Config) server.Config {
	result := server.DefaultConfig()
	result.Scheduler.MaxConcurrent = cfg.MaxConcurrentRequests
	result.Scheduler.MaxQueueSize = cfg.MaxQueueSize
	result.IndexDir = cfg.IndexDir
	result.IndexDiskBudgetBytes = cfg.IndexDiskBudgetBytes
	result.WatchInterval = defaultWatchInterval
	return result
}

// serveConfig 是 serverConfigFrom 的 serve 路径包装：把 --plugins flag 映射为
// server.Config.PluginsDir（DEF-PLUGMOUNT）。空串（缺省）保持字段零值——插件
// 车道整体禁用，装配层零行为（X10：默认关）。replay 离线回放不经此包装，
// 不装配插件：回放必须逐字节重放录制会话，插件子进程是录制之外的外部源。
func serveConfig(cfg config.Config, pluginsDir string) server.Config {
	result := serverConfigFrom(cfg)
	result.PluginsDir = pluginsDir
	return result
}
