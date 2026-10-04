//go:build !windows

package perf

func e2eWindowsProcessTreeResources(rootPID int, stage string) e2eResourceSnapshot {
	return e2eResourceSnapshot{Stage: stage, RootPID: rootPID, Status: "not_verified", Error: "Windows Toolhelp process snapshot is unavailable on this host"}
}
