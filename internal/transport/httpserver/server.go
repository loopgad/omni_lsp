// Package httpserver exposes a read-only HTTP API over the semantic core
// (goal.md C15/X6/P6/P7/N8).
//
// Invariants:
//  1. U3: this package defines a consumer-side Core interface; it never
//     imports internal/runtime/server.
//  2. C15: semantic results are projected to JSON verbatim (all envelope
//     fields preserved); the HTTP body is never a second semantic model.
//  3. N8: the listen address must be loopback-only (see ValidateAddr).
//
// Concurrency: Server is safe for concurrent use (token bucket is mutexed;
// Core is assumed thread-safe per languages.Backend contract).
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// Default rate-limit values used when Options fields are zero.
const (
	DefaultRateLimit = 50.0 // requests per second
	DefaultBurst     = 100
)

// ReadAPIVersion is the §R4 version surface for the HTTP read API
// (docs/versions.md: "HTTP read API | omnilsp.read.v1"). Per §R5 the route
// table is additive-only; removals need a deprecation cycle (§R8).
const ReadAPIVersion = "omnilsp.read.v1"

// readAPIVersionHeader carries ReadAPIVersion on every /api/v1/* response.
const readAPIVersionHeader = "X-OmniLSP-Read-API-Version"

// Core is the consumer-side view of the semantic engine (goal.md U3).
type Core interface {
	Hover(ctx context.Context, uri string, line, column uint32) (identity.SemanticResult[*languages.HoverResult], error)
	Definition(ctx context.Context, uri string, line, column uint32) (identity.SemanticResult[[]languages.Location], error)
	References(ctx context.Context, uri string, line, column uint32, includeDeclaration bool) (identity.SemanticResult[[]languages.Location], error)
	Status() map[string]any
	Ready() bool
}

// Options configures the server. Zero values fall back to defaults.
type Options struct {
	// Addr is the listen address; validated by ValidateAddr and bound by
	// the caller (main), never by this package.
	Addr      string
	RateLimit float64 // requests per second for /api/v1/* (global bucket)
	Burst     int     // bucket capacity

	// Auth gates /api/v1/* behind bearer tokens and per-source limits
	// (§X6). Zero value keeps the local-trust allow-all posture.
	Auth AuthOptions
}

// Server serves the read-only HTTP API. Implementes http.Handler.
type Server struct {
	core   Core
	mux    *http.ServeMux
	bucket *tokenBucket
	auth   http.Handler // nil = no auth wrapper

	// Addr echoes Options.Addr so callers can validate and bind it.
	Addr string
}

// New builds the server and wires its routes.
func New(core Core, opts Options) *Server {
	if opts.RateLimit <= 0 {
		opts.RateLimit = DefaultRateLimit
	}
	if opts.Burst <= 0 {
		opts.Burst = DefaultBurst
	}
	s := &Server{
		core:   core,
		mux:    http.NewServeMux(),
		bucket: newTokenBucket(opts.RateLimit, opts.Burst),
		Addr:   opts.Addr,
	}
	// P7: health reflects only that this process is alive; degraded
	// backends must not flip it.
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /ready", s.handleReady)
	s.mux.HandleFunc("GET /status", s.handleStatus)

	// Rate limiting (N8) applies only to query endpoints. The §R4 version
	// header is stamped at this single wrapper so every rejection produced
	// inside the mux — including 429 from the limiter — carries it. It does
	// not cover 401: ServeHTTP hands the request to the auth wrapper before it
	// ever reaches this mux, so an auth rejection leaves without the header.
	api := func(h http.HandlerFunc) http.Handler {
		return withReadAPIVersion(s.limit(h))
	}
	if opts.Auth.Tokens != nil || opts.Auth.PerSourcePerMinute > 0 {
		s.auth = withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.mux.ServeHTTP(w, r)
		}), opts.Auth, newPerSourceLimiter(opts.Auth.PerSourcePerMinute), nowUnix)
	}
	s.mux.Handle("POST /api/v1/hover", api(s.hover))
	s.mux.Handle("POST /api/v1/definition", api(s.definition))
	s.mux.Handle("POST /api/v1/references", api(s.references))
	return s
}

func nowUnix() int64 { return time.Now().Unix() }

// withReadAPIVersion stamps the §R4 read-API version header on every
// response passing through the /api/v1/* wrapper chain.
func withReadAPIVersion(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(readAPIVersionHeader, ReadAPIVersion)
		h.ServeHTTP(w, r)
	})
}

// ServeHTTP delegates to the routed mux; auth wraps everything when enabled.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.ServeHTTP(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// ValidateAddr reports whether addr binds exclusively to a loopback host
// (127.x / [::1] / "localhost"). N8 safety v1: the HTTP surface must never
// be reachable off-machine.
func ValidateAddr(addr string) error {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing non-loopback listen address %q: N8 requires 127.x/[::1]/localhost", addr)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.core.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ready": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ready": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.core.Status()
	// Inject the §R4 surface into a copy: Core implementations may hand back
	// their own map, and mutating it would corrupt core-internal state.
	out := make(map[string]any, len(st)+1)
	for k, v := range st {
		out[k] = v
	}
	out["readAPIVersion"] = ReadAPIVersion
	writeJSON(w, http.StatusOK, out)
}

// positionRequest is the shared POST body: {uri,line,column[,includeDeclaration]}.
type positionRequest struct {
	URI                string `json:"uri"`
	Line               uint32 `json:"line"`
	Column             uint32 `json:"column"`
	IncludeDeclaration bool   `json:"includeDeclaration"`
}

func decodePosition(w http.ResponseWriter, r *http.Request) (positionRequest, bool) {
	var req positionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	if req.URI == "" {
		http.Error(w, "uri is required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (s *Server) hover(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePosition(w, r)
	if !ok {
		return
	}
	res, err := s.core.Hover(r.Context(), req.URI, req.Line, req.Column)
	writeEnvelope(w, res, err)
}

func (s *Server) definition(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePosition(w, r)
	if !ok {
		return
	}
	res, err := s.core.Definition(r.Context(), req.URI, req.Line, req.Column)
	writeEnvelope(w, res, err)
}

func (s *Server) references(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePosition(w, r)
	if !ok {
		return
	}
	res, err := s.core.References(r.Context(), req.URI, req.Line, req.Column, req.IncludeDeclaration)
	writeEnvelope(w, res, err)
}

// envelopeJSON mirrors identity.SemanticResult field-for-field with stable
// lowercase keys. C15: it is a pure projection — every envelope field is
// preserved, nothing added or dropped.
type envelopeJSON[T any] struct {
	Status              string              `json:"status"`
	Value               T                   `json:"value"`
	Evidence            []identity.Evidence `json:"evidence"`
	Completeness        string              `json:"completeness"`
	InternalDiagnostics []string            `json:"internalDiagnostics"`
}

func completenessString(c identity.Completeness) string {
	switch c {
	case identity.Complete:
		return "complete"
	case identity.IncompleteKnownSubset:
		return "incomplete_known_subset"
	default:
		return "unknown"
	}
}

// writeEnvelope projects a SemanticResult to the response. A nil Value
// marshals as null (Go default for nil pointers/slices). Operational
// failures stay in err -> 500; semantic status lives in the envelope -> 200.
func writeEnvelope[T any](w http.ResponseWriter, r identity.SemanticResult[T], err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, envelopeJSON[T]{
		Status:              r.Status.String(),
		Value:               r.Value,
		Evidence:            r.Evidence,
		Completeness:        completenessString(r.Completeness),
		InternalDiagnostics: r.InternalDiagnostics,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
