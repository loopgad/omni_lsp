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
