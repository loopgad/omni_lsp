package query

import "context"

// State is the §J4 query lifecycle.
type State int

const (
	Absent    State = iota
	Computing       // never stored: in-flight work is tracked in the inflight table
	Ready
	FailedStable    // negative knowledge; cached until a dependency changes
	FailedTransient // never stored: a retryable failure drops the entry instead
	Evicted         // never stored: eviction drops the entry too
)

func (s State) String() string {
	switch s {
	case Absent:
		return "absent"
	case Computing:
		return "computing"
	case Ready:
		return "ready"
	case FailedStable:
		return "failed-stable"
	case FailedTransient:
		return "failed-transient"
	default:
		// Not the Evicted constant: that value is never stored, and
		// naming a stray or out-of-range State "evicted" would send the
		// reader looking at eviction instead of at whatever produced it.
		return "unknown"
	}
}

// ComputeFn executes the actual work: it returns the value plus the set of
// dependencies actually read (may be narrower than declared).
type ComputeFn func(ctx context.Context, bind Bindings) (any, DepSet, error)

// Result carries the computed value with publish-time metadata (§J7).
type Result struct {
	Value       any
	Evidence    string // provenance attached at publish time
	SafetyClass int    // 0 = read-only … higher = more mutation-prone (§B7)
}
