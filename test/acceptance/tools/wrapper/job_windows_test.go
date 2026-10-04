//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	jobTestModeEnv     = "OMNILSP_ACCEPTANCE_JOB_TEST_MODE"
	jobTestNodePathEnv = "OMNILSP_ACCEPTANCE_JOB_TEST_NODE_PATH"
	jobTestPIDFile     = "OMNILSP_ACCEPTANCE_JOB_TEST_PID_FILE"
)

var waitForSingleObject = syscall.NewLazyDLL("kernel32.dll").NewProc("WaitForSingleObject")

func TestKillOnCloseReapsNodeTree(t *testing.T) {
	mode := os.Getenv(jobTestModeEnv)
	if mode == "owner" {
		const script = `const fs=require('node:fs');const {spawn}=require('node:child_process');const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});fs.writeFileSync(process.argv[1],process.pid+'\n'+child.pid+'\n');setInterval(()=>{},1000);`
		cmd := exec.Command(os.Getenv(jobTestNodePathEnv), "-e", script, os.Getenv(jobTestPIDFile))
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := runCommandWithJob(cmd); err != nil {
			t.Errorf("run Node helper in job: %v", err)
		}
		return
	}
	if mode != "" {
		t.Fatalf("unexpected job test mode %q", mode)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the Windows Job Object regression")
	}

	pidFile := filepath.Join(t.TempDir(), "node-pids.txt")
	owner := exec.Command(os.Args[0], "-test.run=^TestKillOnCloseReapsNodeTree$")
	owner.Env = replaceTestEnv(os.Environ(), jobTestModeEnv, "owner")
	owner.Env = replaceTestEnv(owner.Env, jobTestNodePathEnv, nodePath)
	owner.Env = replaceTestEnv(owner.Env, jobTestPIDFile, pidFile)
	owner.Stdout, owner.Stderr = io.Discard, io.Discard
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerWaited := false
	defer func() {
		if !ownerWaited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	var pids []int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			for _, field := range strings.Fields(string(data)) {
				pid, parseErr := strconv.Atoi(field)
				if parseErr == nil {
					pids = append(pids, pid)
				}
			}
			if len(pids) == 2 {
				break
			}
			pids = nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pids) != 2 {
		t.Fatal("Node helper and descendant did not report their process IDs")
	}

	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("terminate wrapper owner: %v", err)
	}
	_ = owner.Wait()
	ownerWaited = true
	for _, pid := range pids {
		if err := waitForProcessExit(pid, 10*time.Second); err != nil {
			t.Errorf("process %d survived wrapper owner termination: %v", pid, err)
		}
	}
}

func replaceTestEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, key+"="+value)
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		handle, err := syscall.OpenProcess(0x00100000|0x00001000, false, uint32(pid))
		if err != nil {
			if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: PID no longer exists.
				return nil
			}
			return err
		}
		result, _, callErr := waitForSingleObject.Call(uintptr(handle), 0)
		_ = syscall.CloseHandle(handle)
		switch result {
		case 0: // WAIT_OBJECT_0
			return nil
		case 258: // WAIT_TIMEOUT
			_ = callErr
			time.Sleep(10 * time.Millisecond)
		default:
			return fmt.Errorf("WaitForSingleObject returned 0x%x: %v", result, callErr)
		}
	}
	return fmt.Errorf("process did not exit within %s", timeout)
}
