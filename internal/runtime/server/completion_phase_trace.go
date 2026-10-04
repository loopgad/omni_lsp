package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	completionPhaseTraceEnv             = "OMNILSP_S18_COMPLETION_PHASE_TRACE"
	completionPhaseTraceRunIDEnv        = "OMNILSP_RUN_ID"
	completionPhaseTraceCandidateSHAEnv = "OMNILSP_CANDIDATE_SHA256"
	maxCompletionPhaseSamples           = 16384
)

type completionPhaseTraceSample struct {
	LanguageID                      string          `json:"language_id"`
	URI                             string          `json:"uri"`
	ParentRequestID                 json.RawMessage `json:"parent_request_id"`
	SchedulePrepareNS               uint64          `json:"schedule_prepare_ns,omitempty"`
	SchedulerSubmitNS               uint64          `json:"scheduler_submit_ns,omitempty"`
	SchedulerQueueWaitNS            uint64          `json:"scheduler_queue_wait_ns,omitempty"`
	DispatcherCallNS                uint64          `json:"dispatcher_call_ns,omitempty"`
	SchedulerResultDeliveryNS       uint64          `json:"scheduler_result_delivery_ns,omitempty"`
	ResponseWriteNS                 uint64          `json:"response_write_ns,omitempty"`
	ResponseOutcome                 string          `json:"response_outcome,omitempty"`
	ServerRoundTripNS               uint64          `json:"server_round_trip_ns,omitempty"`
	SemanticDispatchNS              uint64          `json:"semantic_dispatch_ns"`
	DispatchOutcome                 string          `json:"dispatch_outcome"`
	SnapshotMode                    string          `json:"snapshot_mode"`
	BeginBackendWorkspaceSnapshotNS uint64          `json:"begin_backend_workspace_snapshot_ns"`
	ProviderKind                    string          `json:"provider_kind"`
	ProviderOutcome                 string          `json:"provider_outcome"`
	BackendCompletionListNS         uint64          `json:"backend_completion_list_ns"`
	BackendCompletionFallbackNS     uint64          `json:"backend_completion_fallback_ns,omitempty"`
	ProjectionAttempted             bool            `json:"projection_attempted"`
	ProjectionOutcome               string          `json:"projection_outcome"`
	CompletionProjectionMarshalNS   uint64          `json:"completion_projection_marshal_ns"`
}

type completionPhaseTraceReport struct {
	Version               int                          `json:"version"`
	RunID                 string                       `json:"run_id,omitempty"`
	CandidateSHA256       string                       `json:"candidate_sha256,omitempty"`
	ProcessID             int                          `json:"process_id"`
	CreatedUTC            time.Time                    `json:"created_utc"`
	SampleLimit           int                          `json:"sample_limit"`
	SamplesTruncated      bool                         `json:"samples_truncated"`
	DroppedPendingSamples uint64                       `json:"dropped_pending_samples"`
	Samples               []completionPhaseTraceSample `json:"samples"`
}

type completionPhaseTraceRecorder struct {
	path        string
	sampleLimit int

	mu               sync.Mutex
	samples          []completionPhaseTraceSample
	pending          uint64
	closed           bool
	samplesTruncated bool
	closeOnce        sync.Once
	closeErr         error
}

type completionPhaseTraceSpan struct {
	recorder             *completionPhaseTraceRecorder
	sample               completionPhaseTraceSample
	serverStartedAt      time.Time
	dispatcherReturnedAt time.Time
	serverManaged        bool
}

type completionPhaseTraceContextKey struct{}

func newCompletionPhaseTraceFromEnv() *completionPhaseTraceRecorder {
	path := os.Getenv(completionPhaseTraceEnv)
	if path == "" {
		return nil
	}
	return newCompletionPhaseTraceRecorder(path, maxCompletionPhaseSamples)
}

func newCompletionPhaseTraceRecorder(path string, sampleLimit int) *completionPhaseTraceRecorder {
	if path == "" {
		return nil
	}
	if sampleLimit < 1 {
		sampleLimit = maxCompletionPhaseSamples
	}
	return &completionPhaseTraceRecorder{
		path:        path,
		sampleLimit: sampleLimit,
		samples:     make([]completionPhaseTraceSample, 0, min(sampleLimit, 256)),
	}
}

func (r *completionPhaseTraceRecorder) begin(languageID, uri string, parentRequestID json.RawMessage) *completionPhaseTraceSpan {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.pending++
	return &completionPhaseTraceSpan{
		recorder: r,
		sample: completionPhaseTraceSample{
			LanguageID:      languageID,
			URI:             uri,
			ParentRequestID: cloneCompletionPhaseRequestID(parentRequestID),
		},
	}
}

func (span *completionPhaseTraceSpan) finish() {
	if span == nil || span.recorder == nil {
		return
	}
	r := span.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.pending > 0 {
		r.pending--
	}
	if len(r.samples) >= r.sampleLimit {
		r.samplesTruncated = true
		return
	}
	r.samples = append(r.samples, span.sample)
}

func completionPhaseSpanFromContext(ctx context.Context) *completionPhaseTraceSpan {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(completionPhaseTraceContextKey{}).(*completionPhaseTraceSpan)
	return span
}

func withCompletionPhaseSpan(ctx context.Context, span *completionPhaseTraceSpan) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if span == nil {
		return ctx
	}
	return context.WithValue(ctx, completionPhaseTraceContextKey{}, span)
}

func cloneCompletionPhaseRequestID(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func elapsedNanoseconds(started time.Time) uint64 {
	if started.IsZero() {
		return 0
	}
	ns := time.Since(started).Nanoseconds()
	if ns < 1 {
		// A started phase necessarily consumed positive time, even when the
		// platform clock's nanosecond conversion rounds that interval to zero.
		return 1
	}
	return uint64(ns)
}

// close stops collection and writes one report. Requests that were already in
// a measured dispatch are counted as dropped if they have not recorded by the
// time teardown snapshots the bounded in-memory buffer.
func (r *completionPhaseTraceRecorder) close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		dropped := r.pending
		report := completionPhaseTraceReport{
			Version:               2,
			RunID:                 strings.TrimSpace(os.Getenv(completionPhaseTraceRunIDEnv)),
			CandidateSHA256:       strings.ToLower(strings.TrimPrefix(strings.TrimSpace(os.Getenv(completionPhaseTraceCandidateSHAEnv)), "sha256:")),
			ProcessID:             os.Getpid(),
			CreatedUTC:            time.Now().UTC(),
			SampleLimit:           r.sampleLimit,
			SamplesTruncated:      r.samplesTruncated || dropped > 0,
			DroppedPendingSamples: dropped,
			Samples:               append([]completionPhaseTraceSample(nil), r.samples...),
		}
		r.mu.Unlock()

		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			r.closeErr = fmt.Errorf("marshal completion phase trace: %w", err)
			return
		}
		if err := os.WriteFile(r.path, data, 0600); err != nil {
			r.closeErr = fmt.Errorf("write completion phase trace %q: %w", r.path, err)
		}
	})
	return r.closeErr
}
