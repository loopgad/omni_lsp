package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// fakeCore hardcodes envelope responses and records the last arguments.
type fakeCore struct {
	hoverRes identity.SemanticResult[*languages.HoverResult]
	defRes   identity.SemanticResult[[]languages.Location]
	refRes   identity.SemanticResult[[]languages.Location]
	hoverErr error
	ready    bool

	// status, when set, is returned verbatim so tests can observe whether
	// handlers mutate core-internal state.
	status map[string]any

	lastURI    string
	lastLine   uint32
	lastColumn uint32
	lastIncl   bool
}

func (f *fakeCore) Hover(_ context.Context, uri string, line, col uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	f.lastURI, f.lastLine, f.lastColumn = uri, line, col
	return f.hoverRes, f.hoverErr
}

func (f *fakeCore) Definition(_ context.Context, uri string, line, col uint32) (identity.SemanticResult[[]languages.Location], error) {
	f.lastURI, f.lastLine, f.lastColumn = uri, line, col
	return f.defRes, nil
}

func (f *fakeCore) References(_ context.Context, uri string, line, col uint32, incl bool) (identity.SemanticResult[[]languages.Location], error) {
	f.lastURI, f.lastLine, f.lastColumn, f.lastIncl = uri, line, col, incl
	return f.refRes, nil
}

func (f *fakeCore) Status() map[string]any {
	if f.status != nil {
		return f.status
	}
	return map[string]any{"state": "running", "queue": 3}
}
func (f *fakeCore) Ready() bool { return f.ready }

func newTestServer(fc *fakeCore, opts Options) *httptest.Server {
	return httptest.NewServer(New(fc, opts))
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

// TestEnvelopeRoundTrip asserts all five envelope keys survive projection,
// position arguments reach the Core unchanged, and nil Value -> null.
func TestEnvelopeRoundTrip(t *testing.T) {
	fc := &fakeCore{
		hoverRes: identity.NewExactResult(&languages.HoverResult{Contents: "hover doc"}, nil),
		defRes: identity.NewExactResult([]languages.Location{
			{URI: "file:///x.go", Range: languages.Range{StartLine: 3, StartCharacter: 7}},
		}, nil),
		refRes: identity.NewPartialResult([]languages.Location{{URI: "file:///y.go"}}, nil),
		ready:  true,
	}
	srv := newTestServer(fc, Options{})
	defer srv.Close()

	body := `{"uri":"file:///a.go","line":3,"column":7}`

	t.Run("hover", func(t *testing.T) {
		resp := postJSON(t, srv.URL+"/api/v1/hover", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		m := decodeBody(t, resp)
		for _, key := range []string{"status", "value", "evidence", "completeness", "internalDiagnostics"} {
			if _, ok := m[key]; !ok {
				t.Errorf("envelope key %q missing", key)
			}
		}
		if m["status"] != "exact" || m["completeness"] != "complete" {
			t.Errorf("status/completeness = %v/%v, want exact/complete", m["status"], m["completeness"])
		}
		val, _ := m["value"].(map[string]any)
		if val["Contents"] != "hover doc" {
			t.Errorf("value.Contents = %v", val["Contents"])
		}
	})

	t.Run("definition passes position", func(t *testing.T) {
		resp := postJSON(t, srv.URL+"/api/v1/definition", body)
		m := decodeBody(t, resp)
		locs, ok := m["value"].([]any)
		if !ok || len(locs) != 1 {
			t.Fatalf("value = %#v, want one location", m["value"])
		}
		loc := locs[0].(map[string]any)
		if loc["URI"] != "file:///x.go" {
			t.Errorf("location uri = %v", loc["URI"])
		}
	})

	t.Run("references includeDeclaration", func(t *testing.T) {
		fc.lastIncl = false
		resp := postJSON(t, srv.URL+"/api/v1/references", `{"uri":"file:///a.go","line":3,"column":7,"includeDeclaration":true}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		m := decodeBody(t, resp)
		if !fc.lastIncl {
			t.Error("includeDeclaration not forwarded to Core")
		}
		if fc.lastLine != 3 || fc.lastColumn != 7 || fc.lastURI != "file:///a.go" {
			t.Errorf("position args = (%q,%d,%d)", fc.lastURI, fc.lastLine, fc.lastColumn)
		}
		if m["status"] != "partial" {
			t.Errorf("status = %v, want partial", m["status"])
		}
	})

	t.Run("nil value marshals as null", func(t *testing.T) {
		fc.hoverRes = identity.NewUnavailableResult[*languages.HoverResult](nil)
		resp := postJSON(t, srv.URL+"/api/v1/hover", body)
		m := decodeBody(t, resp)
		if m["value"] != nil {
			t.Errorf("value = %v, want null", m["value"])
		}
		if m["status"] != "unavailable" {
			t.Errorf("status = %v, want unavailable", m["status"])
		}
	})
}

func TestHealthReadyStatus(t *testing.T) {
	fc := &fakeCore{ready: true}
	srv := newTestServer(fc, Options{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %v status=%d", err, resp.StatusCode)
	}
	if m := decodeBody(t, resp); m["ok"] != true {
		t.Errorf("health body = %v", m)
	}

	resp, _ = http.Get(srv.URL + "/status")
	if m := decodeBody(t, resp); m["state"] != "running" || m["queue"] != float64(3) {
		t.Errorf("status body = %v", m)
	}

	// P6: not-ready core -> 503.
	fc.ready = false
	resp, _ = http.Get(srv.URL + "/ready")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("ready status = %d, want 503", resp.StatusCode)
	}
}

// TestReadAPIVersionSurface locks the §R4 version surface mechanically
// (docs/versions.md: "HTTP read API | omnilsp.read.v1"): every /api/v1/*
// response carries the version header — including rate-limit rejections —
// and GET /status exposes the version as a body field via a copy of the
// core's map, never by mutating core-internal state.
func TestReadAPIVersionSurface(t *testing.T) {
	fc := &fakeCore{
		hoverRes: identity.NewExactResult(&languages.HoverResult{}, nil),
		defRes:   identity.NewExactResult([]languages.Location{}, nil),
		refRes:   identity.NewExactResult([]languages.Location{}, nil),
		ready:    true,
	}
	srv := newTestServer(fc, Options{})
	defer srv.Close()

	body := `{"uri":"file:///a.go","line":0,"column":0}`
	for _, route := range []string{"/api/v1/hover", "/api/v1/definition", "/api/v1/references"} {
		resp := postJSON(t, srv.URL+route, body)
		if got := resp.Header.Get(readAPIVersionHeader); got != ReadAPIVersion {
			t.Errorf("%s %s = %q, want %q", route, readAPIVersionHeader, got, ReadAPIVersion)
		}
		resp.Body.Close()
	}

	// The stamp sits outside the limiter, so 429 rejections carry it too.
	limited := newTestServer(fc, Options{RateLimit: 1, Burst: 1})
	defer limited.Close()
	resp := postJSON(t, limited.URL+"/api/v1/hover", body)
	resp.Body.Close()
	rejected := postJSON(t, limited.URL+"/api/v1/hover", body)
	defer rejected.Body.Close()
	if rejected.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second limited request = %d, want 429", rejected.StatusCode)
	}
	if got := rejected.Header.Get(readAPIVersionHeader); got != ReadAPIVersion {
		t.Errorf("429 %s = %q, want %q", readAPIVersionHeader, got, ReadAPIVersion)
	}

	// /status exposes the version via a copy; the core map stays untouched.
	coreStatus := map[string]any{"State": "running", "CacheHits": float64(7)}
	fc.status = coreStatus
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	m := decodeBody(t, resp)
	if m["readAPIVersion"] != ReadAPIVersion {
		t.Errorf("status readAPIVersion = %v, want %q", m["readAPIVersion"], ReadAPIVersion)
	}
	if m["State"] != "running" || m["CacheHits"] != float64(7) {
		t.Errorf("status body lost core fields: %v", m)
	}
	if _, mutated := coreStatus["readAPIVersion"]; mutated {
		t.Error("core.Status() map mutated with readAPIVersion")
	}
}

// TestRateLimit: rate=1 burst=1 -> first request consumes the bucket,
// subsequent immediate requests are rejected with Retry-After.
func TestRateLimit(t *testing.T) {
	fc := &fakeCore{hoverRes: identity.NewExactResult(&languages.HoverResult{}, nil), ready: true}
	srv := newTestServer(fc, Options{RateLimit: 1, Burst: 1})
	defer srv.Close()

	var codes []int
	for i := 0; i < 3; i++ {
		resp := postJSON(t, srv.URL+"/api/v1/hover", `{"uri":"file:///a.go","line":0,"column":0}`)
		codes = append(codes, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests && i == 2 {
			if ra := resp.Header.Get("Retry-After"); ra == "" {
				t.Error("429 missing Retry-After header")
			} else {
				resp.Body.Close()
			}
		}
		resp.Body.Close()
	}
	if codes[0] != http.StatusOK {
		t.Errorf("first request code = %d, want 200", codes[0])
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("third request code = %d, want 429", codes[2])
	}

	// health is exempt from limiting
	resp, _ := http.Get(srv.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("limited /health code = %d, want 200", resp.StatusCode)
	}
}

func TestValidateAddr(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:8080", false},
		{"[::1]:9090", false},
		{"localhost:8080", false},
		{"127.9.9.9:1", false},
		{"0.0.0.0:8080", true},
		{":8080", true},
		{"192.168.1.10:8080", true},
	}
	for _, tc := range cases {
		err := ValidateAddr(tc.addr)
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("ValidateAddr(%q) err = %v, wantErr %v", tc.addr, err, tc.wantErr)
		}
	}
}

func TestRoutingErrors(t *testing.T) {
	fc := &fakeCore{ready: true}
	srv := newTestServer(fc, Options{})
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/nope") // unmatched path -> 404
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/health", nil)
	resp, _ = http.DefaultClient.Do(req) // wrong method on registered route -> 405
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /health = %d, want 405", resp.StatusCode)
	}

	resp = postJSON(t, srv.URL+"/api/v1/hover", `{bad json`) // malformed body -> 400
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", resp.StatusCode)
	}

	resp = postJSON(t, srv.URL+"/api/v1/hover", `{"line":1}`) // empty uri -> 400
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty uri = %d, want 400", resp.StatusCode)
	}

	// Core operational error -> 500.
	fc.hoverErr = fmt.Errorf("boom")
	resp = postJSON(t, srv.URL+"/api/v1/hover", `{"uri":"file:///a.go","line":0,"column":0}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("core error = %d, want 500", resp.StatusCode)
	}
}
