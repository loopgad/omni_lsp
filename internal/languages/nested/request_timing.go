package nested

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

const nestedRPCTimingEnv = "OMNILSP_S18_NESTED_RPC_TRACE"

const (
	maxRequestTimingAggregates = 64
	maxRequestTimingSamples    = 16384
)

type requestTimingParentIDKey struct{}

type requestTimingOutcome string

const (
	requestTimingResponse      requestTimingOutcome = "response"
	requestTimingCanceled      requestTimingOutcome = "canceled"
	requestTimingTimeout       requestTimingOutcome = "timeout"
	requestTimingWriteError    requestTimingOutcome = "write_error"
	requestTimingErrorResponse requestTimingOutcome = "error_response"
)

type requestTimingKey struct {
	method string
	uri    string
}

type requestTimingAggregate struct {
	Method         string `json:"method"`
	URI            string `json:"uri"`
	Count          uint64 `json:"count"`
	TotalDuration  uint64 `json:"total_duration_ns"`
	MaxDuration    uint64 `json:"max_duration_ns"`
	Responses      uint64 `json:"responses"`
	Canceled       uint64 `json:"canceled"`
	TimedOut       uint64 `json:"timed_out"`
	WriteErrors    uint64 `json:"write_errors"`
	ErrorResponses uint64 `json:"error_responses"`
}

type requestTimingSample struct {
	Method          string          `json:"method"`
	URI             string          `json:"uri"`
	Duration        uint64          `json:"duration_ns"`
	WriteDuration   uint64          `json:"write_duration_ns"`
	WaitDuration    uint64          `json:"wait_duration_ns"`
	ParentRequestID json.RawMessage `json:"parent_request_id,omitempty"`
	Outcome         string          `json:"outcome"`
}

type requestTimingReport struct {
	Version          int                      `json:"version"`
	Language         string                   `json:"language"`
	SampleLimit      int                      `json:"sample_limit"`
	SamplesTruncated bool                     `json:"samples_truncated"`
	Aggregates       []requestTimingAggregate `json:"aggregates"`
	Samples          []requestTimingSample    `json:"samples,omitempty"`
}

type requestTimingRecorder struct {
	path             string
	language         string
	mu               sync.Mutex
	aggregates       map[requestTimingKey]*requestTimingAggregate
	samples          []requestTimingSample
	samplesTruncated bool
}

func newRequestTimingRecorder(lang string) *requestTimingRecorder {
	if lang != "c" && lang != "cpp" {
		return nil
	}
	path := os.Getenv(nestedRPCTimingEnv)
	if path == "" {
		return nil
	}
	return &requestTimingRecorder{
		path:       path,
		language:   lang,
		aggregates: make(map[requestTimingKey]*requestTimingAggregate),
		samples:    make([]requestTimingSample, 0, maxRequestTimingSamples),
	}
}

func isNestedRPCTimingMethod(method string) bool {
	return method == "textDocument/completion" || method == "textDocument/documentSymbol"
}

func documentURIFromParams(params json.RawMessage) string {
	var requestParams struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if len(params) == 0 || json.Unmarshal(params, &requestParams) != nil {
		return ""
	}
	return requestParams.TextDocument.URI
}

func (r *requestTimingRecorder) record(method, documentURI string, duration, writeDuration, waitDuration time.Duration, parentRequestID json.RawMessage, outcome requestTimingOutcome) {
	if r == nil || documentURI == "" {
		return
	}
	durationNS := duration.Nanoseconds()
	if durationNS < 0 {
		durationNS = 0
	}
	value := uint64(durationNS)
	writeDurationNS := writeDuration.Nanoseconds()
	if writeDurationNS < 0 {
		writeDurationNS = 0
	}
	writeValue := uint64(writeDurationNS)
	waitDurationNS := waitDuration.Nanoseconds()
	if waitDurationNS < 0 {
		waitDurationNS = 0
	}
	waitValue := uint64(waitDurationNS)

	r.mu.Lock()
	defer r.mu.Unlock()

	key := requestTimingKey{method: method, uri: documentURI}
	aggregate := r.aggregates[key]
	if aggregate == nil && len(r.aggregates) < maxRequestTimingAggregates {
		aggregate = &requestTimingAggregate{Method: method, URI: documentURI}
		r.aggregates[key] = aggregate
	}
	if aggregate != nil {
		aggregate.Count++
		aggregate.TotalDuration += value
		if value > aggregate.MaxDuration {
			aggregate.MaxDuration = value
		}
		switch outcome {
		case requestTimingResponse:
			aggregate.Responses++
		case requestTimingCanceled:
			aggregate.Canceled++
		case requestTimingTimeout:
			aggregate.TimedOut++
		case requestTimingWriteError:
			aggregate.WriteErrors++
		case requestTimingErrorResponse:
			aggregate.ErrorResponses++
		}
	}
	if len(r.samples) < maxRequestTimingSamples {
		r.samples = append(r.samples, requestTimingSample{
			Method: method, URI: documentURI, Duration: value,
			WriteDuration: writeValue, WaitDuration: waitValue,
			ParentRequestID: cloneValidRequestID(parentRequestID), Outcome: string(outcome),
		})
	} else {
		r.samplesTruncated = true
	}
}

func withRequestTimingParentID(ctx context.Context, parentRequestID json.RawMessage) context.Context {
	return context.WithValue(ctx, requestTimingParentIDKey{}, cloneValidRequestID(parentRequestID))
}

func requestTimingParentIDFromContext(ctx context.Context) json.RawMessage {
	if ctx == nil {
		return nil
	}
	parentRequestID, _ := ctx.Value(requestTimingParentIDKey{}).(json.RawMessage)
	return cloneValidRequestID(parentRequestID)
}

func cloneValidRequestID(parentRequestID json.RawMessage) json.RawMessage {
	if len(parentRequestID) == 0 || !json.Valid(parentRequestID) {
		return nil
	}
	return append(json.RawMessage(nil), parentRequestID...)
}

func (r *requestTimingRecorder) flush() error {
	r.mu.Lock()
	report := requestTimingReport{
		Version:          1,
		Language:         r.language,
		SampleLimit:      maxRequestTimingSamples,
		SamplesTruncated: r.samplesTruncated,
		Aggregates:       make([]requestTimingAggregate, 0, len(r.aggregates)),
		Samples:          append([]requestTimingSample(nil), r.samples...),
	}
	for _, aggregate := range r.aggregates {
		report.Aggregates = append(report.Aggregates, *aggregate)
	}
	r.mu.Unlock()

	sort.Slice(report.Aggregates, func(i, j int) bool {
		if report.Aggregates[i].Method != report.Aggregates[j].Method {
			return report.Aggregates[i].Method < report.Aggregates[j].Method
		}
		return report.Aggregates[i].URI < report.Aggregates[j].URI
	})
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nested RPC timing trace: %w", err)
	}
	if err := os.WriteFile(r.path, data, 0600); err != nil {
		return fmt.Errorf("write nested RPC timing trace %q: %w", r.path, err)
	}
	return nil
}
