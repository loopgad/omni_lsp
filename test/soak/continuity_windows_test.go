//go:build soak && windows

package soak

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

const deviceNotifyCallback = 0x00000002

type deviceNotifySubscribeParameters struct {
	callback uintptr
	context  uintptr
}

type suspendResumeGuard interface {
	Check() error
	Close() error
}

type windowsSuspendResumeGuard struct {
	events    suspendResumeEvents
	handle    uintptr
	callback  uintptr
	mu        sync.Mutex
	callbacks *suspendResumeCallbackGate
	closed    bool
}

func TestWindowsSuspendResumeGuardRegistration(t *testing.T) {
	guard, err := newSuspendResumeGuard()
	if err != nil {
		t.Fatalf("register Windows suspend/resume notifications: %v", err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("unregister Windows suspend/resume notifications during cleanup: %v", err)
		}
	})
	if err := guard.Check(); err != nil {
		t.Fatalf("fresh monitor received an unexpected power event: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("unregister Windows suspend/resume notifications: %v", err)
	}
}

var (
	soakPowrprof                             = syscall.NewLazyDLL("powrprof.dll")
	powerRegisterSuspendResumeNotification   = soakPowrprof.NewProc("PowerRegisterSuspendResumeNotification")
	powerUnregisterSuspendResumeNotification = soakPowrprof.NewProc("PowerUnregisterSuspendResumeNotification")
)

func newSuspendResumeGuard() (suspendResumeGuard, error) {
	if err := powerRegisterSuspendResumeNotification.Find(); err != nil {
		return nil, fmt.Errorf("PowerRegisterSuspendResumeNotification is unavailable: %w", err)
	}
	if err := powerUnregisterSuspendResumeNotification.Find(); err != nil {
		return nil, fmt.Errorf("PowerUnregisterSuspendResumeNotification is unavailable: %w", err)
	}
	guard := &windowsSuspendResumeGuard{callbacks: newSuspendResumeCallbackGate()}
	guard.callback = syscall.NewCallback(func(_ uintptr, eventType uintptr, _ uintptr) uintptr {
		if !guard.callbacks.enter() {
			return 0
		}
		defer guard.callbacks.leave()
		guard.events.record(uint32(eventType))
		return 0
	})
	if guard.callback == 0 {
		return nil, fmt.Errorf("create Windows suspend/resume callback")
	}
	params := deviceNotifySubscribeParameters{callback: guard.callback}
	var handle uintptr
	result, _, _ := powerRegisterSuspendResumeNotification.Call(
		deviceNotifyCallback,
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&handle)),
	)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&handle)
	runtime.KeepAlive(guard)
	runtime.KeepAlive(guard.callback)
	if result != 0 {
		return nil, fmt.Errorf("PowerRegisterSuspendResumeNotification failed with Windows error %d", result)
	}
	if handle == 0 {
		return nil, fmt.Errorf("PowerRegisterSuspendResumeNotification returned a null registration handle")
	}
	guard.handle = handle
	return guard, nil
}

func (g *windowsSuspendResumeGuard) Check() error {
	return g.events.check()
}

func (g *windowsSuspendResumeGuard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	result, _, _ := powerUnregisterSuspendResumeNotification.Call(g.handle)
	runtime.KeepAlive(g.callback)
	if result != 0 {
		return fmt.Errorf("PowerUnregisterSuspendResumeNotification failed with Windows error %d", result)
	}
	// Unregistration stops new notifications. Seal callback admission and drain
	// callbacks already in flight before the caller reads the final event latch.
	g.callbacks.closeAndWait()
	g.closed = true
	g.handle = 0
	return nil
}
