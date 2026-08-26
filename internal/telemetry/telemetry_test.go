package telemetry

import (
	"context"
	"testing"
)

func TestCounterIncAndValue(t *testing.T) {
	c := NewCounter("test_counter")
	c.Inc(1)
	c.Inc(5)
	if got := c.Value(); got != 6 {
		t.Errorf("Value() = %d, want 6", got)
	}
	c.Inc(-2)
	if got := c.Value(); got != 4 {
		t.Errorf("Value() = %d, want 4", got)
	}
}

func TestCounterName(t *testing.T) {
	c := NewCounter("requests_handled")
	if c.Name() != "requests_handled" {
		t.Errorf("Name() = %q, want 'requests_handled'", c.Name())
	}
}

func TestTraceIDPropagation(t *testing.T) {
	ctx := context.Background()
	if TraceIDFromContext(ctx) != "" {
		t.Error("empty context should have empty TraceID")
	}

	ctx2 := WithTraceID(ctx, "trace-abc-123")
	if TraceIDFromContext(ctx2) != "trace-abc-123" {
		t.Errorf("got %q, want 'trace-abc-123'", TraceIDFromContext(ctx2))
	}

	// Original context unchanged.
	if TraceIDFromContext(ctx) != "" {
		t.Error("original context should not be modified")
	}
}

func TestRecorder(t *testing.T) {
	r := NewRecorder()

	// All methods should be callable without panic.
	r.RequestHandled()
	r.RequestRejected()
	r.BackendCrashed()
	r.SnapshotEpoch(42)

	// Multiple calls should accumulate.
	r.RequestHandled()
	r.RequestHandled()

	// Verify counters exist and are accessible.
	if r.RequestsHandled == nil {
		t.Error("RequestsHandled should not be nil")
	}
}

func TestRecorderConcurrent(t *testing.T) {
	r := NewRecorder()
	done := make(chan struct{}, 100)
	for i := 0; i < 100; i++ {
		go func() {
			r.RequestHandled()
			r.RequestRejected()
			r.BackendCrashed()
			r.SnapshotEpoch(1)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
	// Should not panic and counters should have values.
	if r.RequestsHandled.Value() != 100 {
		t.Errorf("RequestsHandled = %d, want 100", r.RequestsHandled.Value())
	}
	if r.RequestsRejected.Value() != 100 {
		t.Errorf("RequestsRejected = %d, want 100", r.RequestsRejected.Value())
	}
}
