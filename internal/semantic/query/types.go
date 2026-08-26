package query

import "context"

// State is the §J4 query lifecycle.
type State int

const (
	Absent State = iota
	Computing
	Ready
	FailedStable    // negative knowledge; cached until a dependency changes
	FailedTransient // retried on next Query, never cached
	Evicted
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
		return "evicted"
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
