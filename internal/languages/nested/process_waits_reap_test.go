package nested

// round23 C: Conn.processWaits grew by one entry per Attach (nested.go) and
// no code path ever removed entries — every supervised crash/restart cycle
// leaked a processWaitState for the life of the session. The fix retires the
// entry inside the waiter goroutine (processWaitState's startOnce) after
// cmd.Wait has returned and done is closed. This pin fails while any entry
// from a fully attached AND fully reaped generation is still held.

import (
	"os/exec"
	"testing"
	"time"
)

func TestProcessWaitsEntriesRemovedAfterFullReap(t *testing.T) {
	c := New(Config{Name: "pin", Lang: "pin"})
	const generations = 3
	for i := 0; i < generations; i++ {
		cmd := exec.Command("cmd", "/c", "exit 0")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatalf("generation %d stdin pipe: %v", i, err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("generation %d stdout pipe: %v", i, err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("generation %d start: %v", i, err)
		}
		c.Attach(cmd, stdin, stdout)
		state := c.processWaitState(cmd)
		select {
		case <-state.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("generation %d: child never fully reaped", i)
		}
	}
	// The waiter deletes its map entry after close(done); the delete can lag
	// the done signal by one lock acquisition, so poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.processWaitMu.Lock()
		remaining := len(c.processWaits)
		c.processWaitMu.Unlock()
		if remaining == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %d generations fully attached AND fully reaped, processWaits still holds %d entries", generations, remaining)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
