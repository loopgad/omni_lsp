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
