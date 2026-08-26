// Package telemetry provides lightweight, low-cardinality metrics and trace ID
// propagation for OmniLSP.
//
// Responsibility:
//
//	Collects counters (requests handled/rejected, backend crashes, snapshot epochs)
//	and propagates TraceID through context for request correlation.
//
// Owned mutable state:
//
//	None at package level. Each Counter is isolated and safe for concurrent use.
//
// Concurrency model:
//
//	atomic.Int64 — lock-free, safe for concurrent Inc/Load from any goroutine.
//
// Invariants:
//  1. Metric labels are low-cardinality; TraceID is never exposed as a label.
//  2. TraceID flows only through context, never stored globally.
//  3. No metric name collisions — names are canonical dot-notation strings.
package telemetry

import (
	"context"
	"sync/atomic"
)

type Counter struct {
	value  atomic.Int64
	name   string
	labels []string
}

func NewCounter(name string, labels ...string) *Counter {
	return &Counter{name: name, labels: labels}
}

func (c *Counter) Inc(n int64)  { c.value.Add(n) }
func (c *Counter) Value() int64 { return c.value.Load() }
func (c *Counter) Name() string { return c.name }

type ctxKey struct{}

func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

type Recorder struct {
	RequestsHandled  *Counter
	RequestsRejected *Counter
	BackendCrashes   *Counter
	SnapshotEpochs   *Counter
}

func NewRecorder() *Recorder {
	return &Recorder{
		RequestsHandled:  NewCounter("omnilsp.requests_handled"),
		RequestsRejected: NewCounter("omnilsp.requests_rejected"),
		BackendCrashes:   NewCounter("omnilsp.backend_crashes"),
		SnapshotEpochs:   NewCounter("omnilsp.snapshot_epochs"),
	}
}

func (r *Recorder) RequestHandled()        { r.RequestsHandled.Inc(1) }
func (r *Recorder) RequestRejected()       { r.RequestsRejected.Inc(1) }
func (r *Recorder) BackendCrashed()        { r.BackendCrashes.Inc(1) }
func (r *Recorder) SnapshotEpoch(_ uint64) { r.SnapshotEpochs.Inc(1) }
