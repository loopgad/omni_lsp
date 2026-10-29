package main

import (
	"testing"

	"github.com/omnilsp/omni/internal/config"
)

func TestServerConfigFromKeepsPersistentIndexAndSchedulerConfig(t *testing.T) {
	cfg := config.Config{
		IndexDir: "D:/isolated/index", IndexDiskBudgetBytes: 123456,
		MaxConcurrentRequests: 7, MaxQueueSize: 19,
	}
	got := serverConfigFrom(cfg)
	if got.IndexDir != cfg.IndexDir || got.IndexDiskBudgetBytes != cfg.IndexDiskBudgetBytes ||
		got.Scheduler.MaxConcurrent != cfg.MaxConcurrentRequests || got.Scheduler.MaxQueueSize != cfg.MaxQueueSize {
		t.Fatalf("server config lost CLI/file settings: %+v", got)
	}
}

// TestServeConfigMapsPluginsDir 钉住 --plugins flag 的映射语义：非空目录透传
// 进 server.Config.PluginsDir；空串（缺省）保持零值——插件车道禁用（X10 默认关）。
func TestServeConfigMapsPluginsDir(t *testing.T) {
	cfg := config.Config{}
	if got := serveConfig(cfg, ""); got.PluginsDir != "" {
		t.Fatalf("缺省 --plugins 必须保持零值（禁用），got %q", got.PluginsDir)
	}
	if got := serveConfig(cfg, "D:/plugins"); got.PluginsDir != "D:/plugins" {
		t.Fatalf("--plugins 未透传到 server.Config.PluginsDir，got %q", got.PluginsDir)
	}
}
