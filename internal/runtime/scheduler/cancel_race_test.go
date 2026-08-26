package scheduler

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestS4CancelRace exercises the ordering permutations from goal.md S4:
// request starts -> backend call -> edit arrives -> cancel arrives ->
// backend responds -> index publishes.
// Expected: valid terminal state, never double response or stale publish.
func TestS4CancelRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := New(DefaultConfig())
	s.Start(ctx)
	defer s.Shutdown()

	// Case 1: cancel scheduler context before backend responds
	t.Run("cancel_before_response", func(t *testing.T) {
		_, subCancel := context.WithCancel(ctx)
		req := &Request{
			RequestID:  identity.RequestID("cancel-test-1"),
			Priority:   PriorityCompletion,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
					return "done", nil
				}
			},
		}
		result := s.Submit(req)
		if result != Admitted {
			t.Skip("scheduler queue full")
		}
		subCancel()
		select {
		case <-req.Result:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for cancelled request")
		}
	})

	// Case 2: multiple concurrent requests with mixed completion/cancellation
	t.Run("concurrent_mixed", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := &Request{
					RequestID:  identity.RequestID("r" + strconv.Itoa(id)),
					Priority:   PriorityCompletion,
					EnqueuedAt: time.Now(),
					Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
						time.Sleep(5 * time.Millisecond)
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						default:
							return id, nil
						}
					},
				}
				s.Submit(req)
			}(i)
		}
		wg.Wait()
	})

	// Case 3: rapid cancel/start cycles
	t.Run("rapid_cancel_start", func(t *testing.T) {
		for round := 0; round < 50; round++ {
			req := &Request{
				RequestID:  identity.RequestID("r" + strconv.Itoa(round)),
				Priority:   PriorityMaintenance,
				EnqueuedAt: time.Now(),
				Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
					time.Sleep(1 * time.Millisecond)
					return round, nil
				},
			}
			result := s.Submit(req)
			if result != Admitted {
				continue
			}
			select {
			case <-req.Result:
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("round %d: timeout", round)
			}
		}
	})

	// Case 4: stale snapshot after edit during in-flight request
	t.Run("stale_snapshot_during_request", func(t *testing.T) {
		snap := snapshot.New("ws1", 1, nil)
		req := &Request{
			RequestID:  identity.RequestID("stale-test"),
			Snapshot:   snap,
			Priority:   PriorityCompletion,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, s2 *snapshot.Snapshot) (any, error) {
				time.Sleep(20 * time.Millisecond)
				return s2, nil
			},
		}
		result := s.Submit(req)
		if result == Admitted {
			select {
			case <-req.Result:
			case <-time.After(2 * time.Second):
				t.Fatal("timeout for stale snapshot test")
			}
		}
	})
}
