package perf

import "time"

const e2eS19MaxPrivateMemoryBytes = 8 << 30

type e2eResourceSnapshot struct {
	Stage           string    `json:"stage"`
	SampledAt       time.Time `json:"sampledAt"`
	RootPID         int       `json:"rootPid"`
	RootKind        string    `json:"rootKind"`
	ProcessCount    int       `json:"processCount"`
	WorkingSetBytes uint64    `json:"workingSetBytes"`
	PrivateBytes    uint64    `json:"privateBytes"`
	PrivateMetric   string    `json:"privateBytesMetric,omitempty"`
	HandleCount     uint64    `json:"handleCount"`
	ThreadCount     uint64    `json:"threadCount"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
}
