package main

import (
	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/runtime/server"
	"time"
)

// serverConfigFrom keeps live serving and replay bound to the same configured
// scheduler and persistent-index location.
func serverConfigFrom(cfg config.Config) server.Config {
	result := server.DefaultConfig()
	result.Scheduler.MaxConcurrent = cfg.MaxConcurrentRequests
	result.Scheduler.MaxQueueSize = cfg.MaxQueueSize
	result.IndexDir = cfg.IndexDir
	result.IndexDiskBudgetBytes = cfg.IndexDiskBudgetBytes
	result.WatchInterval = 2 * time.Second
	return result
}
