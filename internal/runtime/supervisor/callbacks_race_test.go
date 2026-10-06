package supervisor

import (
	"sync"
	"testing"
)

// TestG4_RegisterCallbacksRacesWithStateTransitions covers callback
// registration against concurrent state changes. RegisterCallbacks writes
// onStateChange and onEpochChange under mu, so every read of them has to hold
// that same lock; reading them outside it is a data race even though they are
// only ever nil-checked. Under -race this is what fails if the read is
// unguarded.
//
// Start, MarkReady and MarkDegraded all notify, so the re-registering goroutine
// below races the notification path for the whole run.
func TestG4_RegisterCallbacksRacesWithStateTransitions(t *testing.T) {
	s := New(DefaultConfig())
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			s.RegisterCallbacks(
				func(State, State, error) {},
				func(uint64) {},
			)
			s.RegisterCallbacks(nil, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Start()
			s.MarkReady()
			s.MarkDegraded("flaky")
		}
	}()

	wg.Wait()
}
